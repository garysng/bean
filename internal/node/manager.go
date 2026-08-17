// Package node implements noded: sandbox lifecycle management on one node.
package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/obs"
	"github.com/garysng/bean/internal/sbxtoken"
)

// Provisioner assigns and removes one sandbox's networking.
//
// Declared here rather than used as the concrete *network.Provisioner so a test
// can drive the create and destroy ordering without root: the orderings this
// manager is responsible for -- release the slot when setup fails, tear down
// before the sandbox record goes -- are the part that leaks host resources when
// wrong, and none of them need a kernel to check.
type Provisioner interface {
	// Provision reserves a slot and builds the namespace, returning the addresses
	// the runtime attaches to. An error means nothing was left behind.
	Provision(sandboxID string) (*network.Layout, error)
	// Deprovision removes the namespace and returns the slot.
	Deprovision(sandboxID string) error
}

// Sandbox is noded's in-memory record of one sandbox.
type Sandbox struct {
	Spec   *nodev1.SandboxSpec
	Handle *runtime.Handle
	State  runtime.State
	Reason string

	conn         *grpc.ClientConn
	lastActivity time.Time
	inFlight     int // data-plane requests in progress; idle sweep skips these

	// net is the addressing this sandbox was given, or nil on a node without
	// networking. Retained because reaching any port inside the guest needs both
	// halves -- the namespace and the address -- and the address alone is the same
	// for every sandbox on the node.
	net *network.Layout

	// agentToken is the plaintext credential this node presents to the sandbox's
	// agent. Only its hash is given to the guest, so this field is the only copy
	// that can actually authenticate a call.
	//
	// Held in memory and deliberately not persisted: it guards a running agent, and
	// a sandbox that outlives this process is reconciled or dropped rather than
	// re-adopted. Writing it down would create a credential with a longer life than
	// the thing it protects.
	agentToken string
}

// Manager owns all sandboxes on this node.
type Manager struct {
	rt      runtime.Runtime
	metrics *obs.Registry

	// Disk refuses new sandboxes while free space is below a floor. The zero value
	// admits everything. See diskguard.go for why this exists as well as the
	// scheduler's disk commitment rather than instead of it.
	Disk DiskGuard

	// Net gives each sandbox its own namespace, tap and egress rules. Nil means
	// this node has no networking configured, and that path is not a degraded
	// mode: sandboxes ran with no interface at all before this existed, and a node
	// without the flags set must behave exactly as it did then rather than refuse
	// every create. So every use of this is guarded, and the guard is the
	// behaviour rather than a precaution.
	Net Provisioner

	mu        sync.Mutex
	sandboxes map[string]*Sandbox

	stopCh chan struct{}

	// statusKick asks the status-report loop to send now rather than wait for its
	// slow periodic tick. A create/destroy/pause/resume changes what the node
	// holds, and the ops view of disk usage should reflect that in seconds, not at
	// the next 60s floor. Buffered with depth 1 and sent non-blockingly (kick), so
	// a burst of lifecycle events coalesces into a single pending report and no
	// caller ever blocks on a report in flight. The periodic tick remains the
	// backstop that covers a dropped kick.
	statusKick chan struct{}
}

func NewManager(rt runtime.Runtime) *Manager {
	m := &Manager{
		rt:         rt,
		metrics:    obs.NewRegistry(),
		sandboxes:  map[string]*Sandbox{},
		stopCh:     make(chan struct{}),
		statusKick: make(chan struct{}, 1),
	}
	// The runtime reports its own sub-phases through the manager, so runtime_create
	// decomposes instead of being one opaque number. Attached here rather than passed
	// to the runtime's constructor because the runtime is built before the manager
	// exists, and the histogram belongs to the manager.
	//
	// Type-asserted rather than added to the Runtime interface: only the microVM tier
	// has steps worth naming, and widening the interface would oblige the local tier
	// to report phases it does not have.
	if pr, ok := rt.(interface {
		SetPhaseObserver(func(context.Context, string, time.Duration))
	}); ok {
		pr.SetPhaseObserver(m.observePhase)
	}
	// The image provider reports its own steps the same way. Set on the package rather
	// than an instance because a rootfs release is a closure captured at Prepare time,
	// and there is one provider per node either way.
	image.ObservePhase = m.observePhase
	go m.idleLoop()
	return m
}

// Metrics exposes the node's registry for the /metrics endpoint.
func (m *Manager) Metrics() *obs.Registry { return m.metrics }

// StatusKick is the channel the status-report loop selects on to send ahead of
// its periodic tick. It fires after a lifecycle change (see kick).
func (m *Manager) StatusKick() <-chan struct{} { return m.statusKick }

// kick nudges the status-report loop to send now. Non-blocking: the channel is
// buffered to depth 1, so if a report is already pending the kick is dropped and
// the pending one covers this change too -- coalescing a burst of creates into a
// single report. A lifecycle path calls this after it has succeeded, never
// before, so the report reflects committed state.
func (m *Manager) kick() {
	select {
	case m.statusKick <- struct{}{}:
	default:
	}
}

// observePhase records one create-phase duration. Phase names mirror the
// cold-start budget in docs/security-and-startup.md B1.
//
// The same call feeds the histogram and the span, so a phase cannot appear in
// one and be missing from the other. Recording them separately is how the two
// drift: someone adds a phase, updates the metric, and the trace keeps a gap
// that looks like idle time.
func (m *Manager) observePhase(ctx context.Context, phase string, d time.Duration) {
	m.metrics.ObserveDuration("bean_node_create_phase_seconds",
		"Sandbox create latency by phase.",
		map[string]string{"phase": phase, "runtime": m.rt.Name()}, d)
	trace.SpanFromContext(ctx).AddEvent("phase."+phase,
		trace.WithAttributes(attribute.Int64("duration_ms", d.Milliseconds())))
}

func (m *Manager) Close() {
	close(m.stopCh)
	m.mu.Lock()
	ids := make([]string, 0, len(m.sandboxes))
	for id := range m.sandboxes {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Destroy(context.Background(), id, true)
	}
}

// Create creates a sandbox and waits for the agent to be healthy.
func (m *Manager) Create(ctx context.Context, spec *nodev1.SandboxSpec) (sb *Sandbox, createErr error) {
	if spec.GetSandboxId() == "" {
		return nil, fmt.Errorf("sandbox_id required")
	}
	ctx, span := obs.Tracer("noded").Start(ctx, "node.Create",
		trace.WithAttributes(
			attribute.String(obs.AttrSandbox, spec.GetSandboxId()),
			attribute.String(obs.AttrImage, spec.GetImage()),
		))
	defer span.End()

	// Checked before the slot is reserved, so a refused create leaves no trace to
	// clean up. Refusing here rather than in the scheduler is deliberate: the node
	// is the only party that can see its own filesystem, and the space at risk
	// includes things the scheduler never accounted for — base images, the snapshot
	// cache, anything else sharing the volume.
	if err := m.Disk.Admit(); err != nil {
		m.metrics.IncCounter("bean_node_creates_refused_total",
			"Creates refused to protect running sandboxes.",
			map[string]string{"reason": "disk_pressure"}, 1)
		return nil, err
	}

	m.mu.Lock()
	if _, exists := m.sandboxes[spec.SandboxId]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s already exists", spec.SandboxId)
	}
	// Reserve the slot to make Create idempotent-safe under concurrency.
	sb = &Sandbox{Spec: spec, State: runtime.StateStarting, lastActivity: time.Now()}
	m.sandboxes[spec.SandboxId] = sb
	m.mu.Unlock()

	rspec := specToRuntime(spec)

	createStart := time.Now()
	outcome := "error"
	defer func() {
		m.metrics.IncCounter("bean_node_creates_total",
			"Sandbox creates handled by this node.",
			map[string]string{"outcome": outcome, "runtime": m.rt.Name()}, 1)
		m.observePhase(ctx, "total", time.Since(createStart))
		// A failed create used to increment a counter and say nothing. Measured: a
		// create against a misconfigured overlaybd backend hung and returned
		// "TIMEOUT: stream terminated by RST_STREAM" to the caller, while noded's
		// log held 15 lines, all from startup. The node is the only party that knows
		// which step failed, so a create that fails without saying why leaves the
		// cause reachable from nowhere.
		// Read from the named return rather than captured at each failure site. There
		// are six error returns in this function and several wrap; assigning at each
		// would mean remembering to, and a forgotten one is silent in exactly the way
		// this whole block exists to prevent.
		if outcome != "success" {
			slog.Error("sandbox create failed", logging.KeySandbox, spec.GetSandboxId(),
				logging.KeyError, createErr, "image", spec.GetImage(),
				"elapsed", time.Since(createStart).Round(time.Millisecond))
		}
	}()

	// Networking is built before the runtime starts, because the tap has to exist
	// before Firecracker is told to attach to it: the network-interface endpoint is
	// pre-boot only, so a guest that starts without one has no NIC for the rest of
	// its life and the symptom appears much later as pip and git failing inside the
	// sandbox (fc_linux.go, configureAndBoot).
	//
	// A failure here fails the create. Letting it continue would produce exactly
	// that sandbox -- running, believing it has a network, with nothing to attach
	// to -- and the whole reason this module is not shipped half done
	// (docs/network.md section 7) is that such a sandbox makes people doubt their
	// own code rather than the platform.
	if m.Net != nil {
		netStart := time.Now()
		layout, err := m.Net.Provision(spec.SandboxId)
		m.observePhase(ctx, "network_setup", time.Since(netStart))
		if err != nil {
			m.dropFailed(spec.SandboxId)
			return nil, fmt.Errorf("sandbox %s: %w", spec.SandboxId, err)
		}
		rspec.Network = layout

		// Minted here, inside the networking branch, because the credential exists to
		// guard an agent that the sandbox itself can dial. Without networking the
		// agent is on a Unix socket outside the guest's mount namespace and nothing
		// inside can reach it, so a token there would be ceremony -- and issuing one
		// anyway would mean the "no credential configured" state stopped being a
		// signal that something is wrong.
		token, terr := sbxtoken.New()
		if terr != nil {
			m.dropFailed(spec.SandboxId)
			return nil, fmt.Errorf("sandbox %s: mint agent token: %w", spec.SandboxId, terr)
		}
		// The plaintext stays on the node and the guest is given only the hash, which
		// is what makes the value in MMDS safe for the sandbox's own root to read.
		rspec.AgentTokenHash = sbxtoken.Hash(token)
		m.mu.Lock()
		if cur, ok := m.sandboxes[spec.SandboxId]; ok {
			cur.agentToken = token
			cur.net = layout
		}
		m.mu.Unlock()
	}

	rtStart := time.Now()
	rtCtx, rtSpan := obs.Tracer("noded").Start(ctx, "runtime.Create",
		trace.WithAttributes(obs.Phase("runtime_create")))
	handle, err := m.createOrRestoreWarm(rtCtx, rspec, spec.GetImage())
	obs.Fail(rtCtx, err)
	rtSpan.End()
	m.observePhase(ctx, "runtime_create", time.Since(rtStart))
	if err != nil {
		m.dropFailed(spec.SandboxId)
		return nil, err
	}

	conn, err := m.dialAgent(ctx, spec.SandboxId, handle)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	sb.Handle = handle
	sb.conn = conn
	sb.State = runtime.StateRunning
	m.mu.Unlock()
	outcome = "success"
	// The node now holds one more sandbox; nudge a status report so the ops disk
	// view reflects it in seconds rather than at the next periodic tick.
	m.kick()

	if spec.AutoStartCmd {
		m.startUserProcess(ctx, conn, spec)
	}
	return sb, nil
}

// startUserProcess starts what the image and the request together say to run.
//
// The image's own configuration is the reason this is not simply a call with the
// request's fields: an image declares ENV, ENTRYPOINT, CMD and WORKDIR, and an
// image whose config is ignored starts in the wrong directory or without the PATH
// its entrypoint expects. That failure is silent -- the process starts, and only
// its behaviour is wrong -- so it is worth resolving even though nothing errors
// when it is skipped.
//
// Fire-and-forget: a failure to start the user process is logged and does not fail
// the create, because the sandbox itself is up and exec still works. That was the
// behaviour before and callers depend on it.
func (m *Manager) startUserProcess(ctx context.Context, conn *grpc.ClientConn, spec *nodev1.SandboxSpec) {
	proc, err := m.resolveProcess(spec)
	if err != nil {
		slog.Error("autoStartCmd config unreadable", logging.KeySandbox, spec.SandboxId,
			logging.KeyError, err)
		return
	}
	// Nothing to start is not a failure. An image with no Entrypoint or Cmd and a
	// request that names none is a sandbox meant to be driven by exec.
	if len(proc.Argv) == 0 {
		return
	}

	// Argv is sent as Cmd with Entrypoint empty rather than split back apart: the
	// merge has already applied the rule that distinguishes them, and the agent
	// concatenates the two in the same order (beand/server.go). Splitting here would
	// mean two places had to agree on where the boundary fell.
	_, err = agentv1.NewAgentServiceClient(conn).StartUserProcess(ctx,
		&agentv1.StartUserProcessRequest{
			Cmd:     proc.Argv,
			Env:     proc.Env,
			Workdir: proc.Workdir,
		})
	if err != nil {
		slog.Error("autoStartCmd failed", logging.KeySandbox, spec.SandboxId, logging.KeyError, err)
	}
}

// resolveProcess merges the image's recorded configuration with the request.
//
// A runtime that cannot report image configs -- the local dev tier, which runs a
// host binary and has no image -- yields the request alone, which is what it did
// before configs were recorded.
func (m *Manager) resolveProcess(spec *nodev1.SandboxSpec) (image.Process, error) {
	reader, ok := m.rt.(runtime.ImageConfigReader)
	if !ok {
		return image.MergeConfig(nil, spec.Cmd, spec.Env, ""), nil
	}
	// The base is named by an OCI reference on a cold start, or by a filesystem
	// manifest digest for a sandbox created from a template or restored from a
	// snapshot. Both carry the same recorded config; passing whichever is set lets
	// a template-created sandbox inherit its ENV/ENTRYPOINT rather than booting bare.
	base := spec.GetImage()
	if base == "" {
		base = spec.GetFsManifestDigest()
	}
	cfg, err := reader.ImageConfig(base)
	if err != nil {
		return image.Process{}, err
	}
	return image.MergeConfig(cfg, spec.Cmd, spec.Env, ""), nil
}

func waitHealthy(ctx context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	c := agentv1.NewAgentServiceClient(conn)
	deadline := time.Now().Add(timeout)
	for {
		hctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := c.Health(hctx, &agentv1.HealthRequest{})
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent not healthy after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// dropFailed removes a sandbox whose creation failed. The runtime has
// already been cleaned up, so keeping a FAILED entry would leak memory
// with no way to reclaim it.
//
// The namespace goes with it. Every path that abandons a half-made sandbox comes
// through here, so releasing the slot here rather than at each call site is what
// makes it impossible to add a new failure path that leaks one -- and a leaked
// slot is invisible until the node refuses a create at a count nobody can
// explain.
func (m *Manager) dropFailed(id string) {
	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Outside the lock: teardown shells out to ip and iptables, and holding the
	// manager's mutex across that would stall every other sandbox operation on the
	// node for the duration.
	m.releaseNetwork(id)
}

// releaseNetwork tears down a sandbox's networking, if it had any.
//
// Failures are logged rather than returned. Both callers are already unwinding --
// a create that failed, or a destroy whose outcome is reported separately -- and
// there is nothing further either could do. It is not silent: what is left behind
// is a namespace, which the allocator sees on the next Reserve and refuses to
// hand out, so the cost of a failed teardown is a lost slot rather than two
// sandboxes sharing addresses.
func (m *Manager) releaseNetwork(id string) {
	if m.Net == nil {
		return
	}
	if err := m.Net.Deprovision(id); err != nil {
		slog.Error("sandbox network teardown failed; its slot stays occupied "+
			"until this node restarts", logging.KeySandbox, id, logging.KeyError, err)
	}
}

// Get returns the sandbox or nil.
func (m *Manager) Get(id string) *Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxes[id]
}

// SpecOf returns the sandbox spec under lock (nil if absent).
func (m *Manager) SpecOf(id string) *nodev1.SandboxSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		return sb.Spec
	}
	return nil
}

// StatusOf returns a snapshot of the sandbox status (nil if absent).
func (m *Manager) StatusOf(id string) *nodev1.SandboxStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return nil
	}
	st := &nodev1.SandboxStatus{
		SandboxId:        id,
		State:            string(sb.State),
		Reason:           sb.Reason,
		LastActivityUnix: sb.lastActivity.Unix(),
	}
	if sb.Handle != nil {
		st.StartedAtUnix = sb.Handle.StartedAt.Unix()
	}
	return st
}

// StateOf returns the sandbox state under lock ("" if absent).
func (m *Manager) StateOf(id string) runtime.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		return sb.State
	}
	return ""
}

// ErrSandboxNotFound reports an unknown sandbox id (maps to gRPC NotFound).
var ErrSandboxNotFound = errors.New("sandbox not found")

// AgentConn returns a live agent connection for RUNNING sandboxes,
// transparently resuming PAUSED ones (wake-on-request). The returned
// release func must be called when the data-plane request finishes; it
// clears the in-flight marker that keeps the idle sweep from pausing or
// killing a sandbox mid-request.
func (m *Manager) AgentConn(ctx context.Context, id string) (*grpc.ClientConn, func(), error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	state := sb.State
	conn := sb.conn
	sb.lastActivity = time.Now()
	sb.inFlight++
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.sandboxes[id]; ok {
			if cur.inFlight > 0 {
				cur.inFlight--
			}
			cur.lastActivity = time.Now()
		}
	}

	switch state {
	case runtime.StateRunning:
		return conn, release, nil
	case runtime.StatePaused:
		if err := m.Resume(ctx, id); err != nil {
			release()
			return nil, nil, fmt.Errorf("wake: %w", err)
		}
		return conn, release, nil
	default:
		release()
		return nil, nil, fmt.Errorf("sandbox %s not runnable (state=%s)", id, state)
	}
}

func (m *Manager) Destroy(ctx context.Context, id string, force bool) error {
	// Flushing happens here, while the agent is still reachable, and replaces
	// waiting for the guest to shut itself down.
	//
	// The runtime used to ask the guest to power off over ACPI and wait up to
	// five seconds for it. That wait could never succeed: the guest kernel is
	// built without CONFIG_ACPI_BUTTON, so nothing in the guest receives the
	// event, and the agent is PID 1 with no signal handler. Every destroy paid
	// the full timeout — measured at 5001ms of a 5335ms destroy — to accomplish
	// nothing. Asking the agent to sync achieves the actual goal (the writable
	// layer on the host matches what the sandbox wrote) and confirms it rather
	// than assuming it happened.
	// Timed because the destroy phase below covers only rt.Destroy, so a slow flush
	// or a slow network teardown was invisible while still being paid by the caller.
	// Measured at 256 concurrent creates, destroy was 2.5-3.1s -- worse than create --
	// and nothing said which of the three parts that was.
	flushStart := time.Now()
	if !force {
		if err := m.flushBeforeDestroy(ctx, id); err != nil {
			// A sandbox being destroyed cannot be kept alive because its
			// filesystem would not flush, so this is reported and not fatal.
			logging.From(ctx).Warn("flush before destroy failed",
				logging.KeySandbox, id, logging.KeyError, err)
		}
	}

	m.observePhase(ctx, "destroy_flush", time.Since(flushStart))

	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.sandboxes, id)
	m.mu.Unlock()

	if sb.conn != nil {
		_ = sb.conn.Close()
	}
	start := time.Now()
	dCtx, dSpan := obs.Tracer("noded").Start(ctx, "runtime.Destroy",
		trace.WithAttributes(obs.Phase("destroy"),
			attribute.String(obs.AttrSandbox, id),
			attribute.Bool("force", force)))
	err := m.rt.Destroy(dCtx, id, force)
	obs.Fail(dCtx, err)
	dSpan.End()
	m.observePhase(ctx, "destroy", time.Since(start))

	// After the runtime, not before: the VMM has the tap open, and removing the
	// namespace from under a live Firecracker leaves it with a device that has gone
	// away rather than shutting it down.
	//
	// Unconditional on the runtime's outcome. A destroy that failed still had its
	// record deleted above, so nothing will ever ask about this sandbox again --
	// skipping the teardown because the runtime errored would leak the namespace and
	// its index permanently, which is the loop-device failure (GitHub #16) in a
	// resource whose reuse gives two sandboxes the same addresses.
	netStart := time.Now()
	m.releaseNetwork(id)
	m.observePhase(ctx, "destroy_network", time.Since(netStart))

	m.metrics.IncCounter("bean_node_destroys_total", "Sandbox destroys handled by this node.",
		map[string]string{"outcome": boolOutcome(err == nil), "runtime": m.rt.Name()}, 1)
	// The record is gone regardless of the runtime's outcome, so the node holds
	// less now; report it. Kicked even on a runtime error, matching the
	// unconditional record delete above.
	m.kick()
	return err
}

func boolOutcome(ok bool) string {
	if ok {
		return "success"
	}
	return "error"
}

func (m *Manager) Pause(ctx context.Context, id string) error {
	// Claim the transition under lock so concurrent pauses cannot both proceed.
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	if sb.State != runtime.StateRunning {
		state := sb.State
		m.mu.Unlock()
		return fmt.Errorf("sandbox %s not RUNNING (state=%s)", id, state)
	}
	sb.State = runtime.StatePausing
	m.mu.Unlock()

	if err := m.rt.Pause(ctx, id); err != nil {
		m.mu.Lock()
		if cur, ok := m.sandboxes[id]; ok && cur.State == runtime.StatePausing {
			cur.State = runtime.StateRunning
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = runtime.StatePaused
	}
	m.mu.Unlock()
	m.kick()
	return nil
}

// Snapshot writes a checkpoint of the sandbox to w. The sandbox is frozen
// for the duration so the checkpoint is internally consistent, then
// returned to its previous state — a snapshot is not supposed to disturb a
// running workload.
//
// opts decides whether guest memory travels with it, which is the difference
// between resuming the guest and rebooting onto its filesystem.
func (m *Manager) Snapshot(ctx context.Context, id string, w io.Writer,
	opts runtime.CheckpointOptions) (runtime.CheckpointResult, error) {
	// Claim the transition under lock, remembering where to go back to.
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return runtime.CheckpointResult{}, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	prev := sb.State
	if prev != runtime.StateRunning && prev != runtime.StatePaused {
		m.mu.Unlock()
		return runtime.CheckpointResult{}, fmt.Errorf("sandbox %s is %s; snapshot needs RUNNING or PAUSED", id, prev)
	}
	sb.State = runtime.StateSnapshotting
	m.mu.Unlock()

	restore := func() {
		m.mu.Lock()
		if cur, ok := m.sandboxes[id]; ok && cur.State == runtime.StateSnapshotting {
			cur.State = prev
		}
		m.mu.Unlock()
	}

	// A checkpoint without guest memory has to flush the guest first, and it has
	// to happen before the pause while the agent can still run.
	//
	// Pausing stops the vCPUs but leaves the guest's page cache dirty, so reading
	// the block device from the host misses whatever the sandbox wrote most
	// recently. With memory that does not matter — the dirty pages travel inside
	// the memory image and the guest flushes them after it resumes. Without
	// memory they are simply lost: measured as a snapshot whose rootfs extents
	// were empty and a restore that could not find a file written seconds before.
	if !opts.IncludeMemory && prev == runtime.StateRunning {
		// Not syncGuest: that goes through AgentConn, which refuses a sandbox
		// whose state is no longer RUNNING — and by this point the state is
		// SNAPSHOTTING, claimed a few lines above. The first version of this
		// used it and the sync silently failed every time, leaving the data
		// loss it was meant to prevent.
		if err := m.syncViaAgent(ctx, id); err != nil {
			// Reported rather than fatal: the checkpoint is still usable, just
			// possibly missing the most recent writes, and failing the snapshot
			// outright would be a worse outcome than an incomplete one the
			// caller is told about.
			logging.From(ctx).Warn("guest sync before memoryless snapshot failed",
				logging.KeySandbox, id, logging.KeyError, err)
		}
	}

	// Freeze a running sandbox so the filesystem is not moving underneath
	// the checkpoint; an already-paused one needs no extra work.
	if prev == runtime.StateRunning {
		if err := m.rt.Pause(ctx, id); err != nil {
			restore()
			return runtime.CheckpointResult{}, fmt.Errorf("freeze for snapshot: %w", err)
		}
	}

	start := time.Now()
	res, err := m.rt.Checkpoint(ctx, id, w, opts)
	m.observePhase(ctx, "checkpoint", time.Since(start))
	m.metrics.IncCounter("bean_node_snapshots_total",
		"Snapshots taken on this node.",
		map[string]string{"outcome": boolOutcome(err == nil), "runtime": m.rt.Name()}, 1)

	// Unfreeze before reporting, so a checkpoint error still leaves the
	// sandbox running rather than silently stuck.
	if prev == runtime.StateRunning {
		if rerr := m.rt.Resume(ctx, id); rerr != nil {
			slog.Error("resume after snapshot failed", logging.KeySandbox, id, logging.KeyError, rerr)
		}
	}

	// Taking a snapshot resets the guest's transport, so the cached connection
	// is dead even though the sandbox is running again. Reconnecting here keeps
	// that a detail of snapshotting rather than an error the next exec reports.
	//
	// Only for a source that is running again. Reconnecting waits for the agent
	// to answer a health check, and an agent inside a sandbox that was already
	// PAUSED when the snapshot began is not scheduled to answer one — so this
	// spent the full agentReadyTimeout failing, on every snapshot of a paused
	// sandbox, and then logged an error for a sandbox that was behaving
	// correctly. Resuming re-dials on its own, which is the point at which the
	// agent can respond.
	if prev == runtime.StateRunning {
		if rerr := m.redialAgent(ctx, id); rerr != nil {
			slog.Error("reconnect after snapshot failed", logging.KeySandbox, id, logging.KeyError, rerr)
		}
	}

	restore()
	if err != nil {
		return runtime.CheckpointResult{}, fmt.Errorf("checkpoint: %w", err)
	}
	return res, nil
}

// syncGuest asks the agent to flush the guest's filesystem, so what the host
// then reads from the block device includes everything the sandbox wrote.
func (m *Manager) syncGuest(ctx context.Context, id string) error {
	conn, release, err := m.AgentConn(ctx, id)
	if err != nil {
		return err
	}
	defer release()

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := agentv1.NewAgentServiceClient(conn).Exec(syncCtx, &commonv1.ExecRequest{
		SandboxId: id,
		Cmd:       []string{"/bin/sh", "-c", "sync"},
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sync exited %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// flushBeforeDestroy asks a running sandbox to flush its filesystem.
//
// A paused sandbox is skipped: it has written nothing since it was paused, and
// waking one in order to tear it down would cost more than the flush saves.
func (m *Manager) flushBeforeDestroy(ctx context.Context, id string) error {
	m.mu.Lock()
	running := false
	if sb, ok := m.sandboxes[id]; ok {
		running = sb.State == runtime.StateRunning
	}
	m.mu.Unlock()
	if !running {
		return nil
	}
	return m.syncViaAgent(ctx, id)
}

// syncViaAgent tells the guest to flush its page cache, so what the host reads
// from the block device includes everything the sandbox wrote.
//
// It takes the connection directly instead of going through AgentConn, which
// both refuses a sandbox that is no longer RUNNING and transparently resumes a
// paused one. Neither behaviour suits a caller that has already claimed a state
// transition — the snapshot path is SNAPSHOTTING by the time it needs this, and
// went unsynced for exactly that reason before.
func (m *Manager) syncViaAgent(ctx context.Context, id string) error {
	m.mu.Lock()
	var conn *grpc.ClientConn
	if sb, ok := m.sandboxes[id]; ok {
		conn = sb.conn
	}
	m.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("sandbox %s has no agent connection", id)
	}

	// The bound is short on purpose: this is best-effort durability, and a guest
	// that cannot answer promptly must not reintroduce a multi-second stall on
	// either the teardown or the snapshot path.
	syncCtx, cancel := context.WithTimeout(ctx, guestSyncTimeout)
	defer cancel()
	// `sync -f /` rather than `sync`, because the two are not the same guarantee and the
	// difference cost a 1-in-8 silent data loss on restore.
	//
	// Plain `sync` walks every filesystem and returns when writeback has been *started*,
	// without ordering a file's data block against the inode that references it. So a
	// checkpoint could capture an inode pointing at a block whose contents were not on the
	// device yet: measured as a restored guest reading an empty file while the restored block
	// device, read from the host at the same offset, correctly served the bytes. The data was
	// in the sealed layer, correctly mapped -- the guest simply did not know to read it.
	//
	// `sync -f` is syncfs(2) on the root's mount, which does order them. Verified on hardware:
	// 8 of 8 cycles pass with it against 20 of 23 before.
	//
	// Falls back to plain `sync` if the guest's coreutils lacks -f, so an image without it is
	// no worse off than before rather than failing the flush outright.
	res, err := agentv1.NewAgentServiceClient(conn).Exec(syncCtx, &commonv1.ExecRequest{
		SandboxId: id,
		Cmd:       []string{"/bin/sh", "-c", guestFlushCommand},
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sync exited %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// BuildImage builds a base image on this node.
//
// Cancelling ctx stops the build: the runtime runs its builder under it, so the
// context is the only handle a caller needs. That is also why the outcome label
// distinguishes a cancelled build from a failed one — a rate of builds someone
// stopped on purpose says nothing about whether this node's BuildKit is healthy,
// and counting the two together is how a broken builder hides.
func (m *Manager) BuildImage(ctx context.Context, req runtime.BuildRequest) (runtime.BuildResult, error) {
	builder, ok := m.rt.(runtime.ImageBuilder)
	if !ok {
		return runtime.BuildResult{}, fmt.Errorf("runtime %s cannot build images", m.rt.Name())
	}
	start := time.Now()
	res, err := builder.BuildImage(ctx, req)
	m.observePhase(ctx, "image_build", time.Since(start))
	m.metrics.IncCounter("bean_node_image_builds_total",
		"Image builds on this node.",
		map[string]string{"outcome": buildOutcome(ctx, err), "runtime": m.rt.Name()}, 1)
	return res, err
}

// buildOutcome labels how a build ended. The context is consulted rather than
// only the error because a killed subprocess reports an exit status, not a
// cancellation: without this a stopped build counts as a failure.
func buildOutcome(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return "success"
	case ctx.Err() != nil:
		return "cancelled"
	default:
		return "error"
	}
}

// CachedImages reports the images this node holds, with the size and digest of
// each.
//
// A runtime with no image cache returns nothing rather than an error: the local
// tier runs a host binary, so there is genuinely nothing cached, and a status
// report should not fail over it.
func (m *Manager) CachedImages() map[string]image.CachedImage {
	lister, ok := m.rt.(runtime.ImageLister)
	if !ok {
		return nil
	}
	cached, err := lister.CachedImages()
	if err != nil {
		slog.Error("cannot list cached images", logging.KeyError, err)
		return nil
	}
	return cached
}

// PrewarmImage makes an image ready on this node.
//
// Not every runtime has a notion of a cached image — the local tier runs a host
// binary — so a runtime that cannot warm reports success rather than an error:
// there is nothing to prepare, which is the same outcome the caller wanted.
func (m *Manager) PrewarmImage(ctx context.Context, imageRef string) error {
	warmer, ok := m.rt.(runtime.ImageWarmer)
	if !ok {
		return nil
	}
	start := time.Now()
	err := warmer.PrewarmImage(ctx, imageRef)
	m.observePhase(ctx, "image_prewarm", time.Since(start))
	m.metrics.IncCounter("bean_node_image_prewarms_total",
		"Image prewarm attempts on this node.",
		map[string]string{"outcome": boolOutcome(err == nil), "runtime": m.rt.Name()}, 1)
	if err != nil {
		return err
	}
	// Preparing the image file removed the pull. It did not remove the boot: a
	// create against a fully prewarmed image still boots a kernel and still costs
	// the ~5 CPU-seconds that put the throughput ceiling at cores/5. Capturing one
	// booted guest is what removes that, so it happens here rather than being a
	// separate operation an operator has to know to run.
	//
	// Reported but not returned. The image is genuinely prewarmed at this point, and
	// the caller asked for that; failing the whole prewarm because the optimisation
	// on top of it did not work would turn a slower create into no create at all.
	if werr := m.warmSnapshotFor(ctx, imageRef); werr != nil {
		slog.Warn("image is prewarmed but has no warm snapshot; creates from it "+
			"will boot", logging.KeyImage, imageRef, logging.KeyError, werr)
	}
	return nil
}

// warmSnapshotFor boots one sandbox from an image, waits for its agent, and
// checkpoints it so later creates restore instead of booting.
//
// The sandbox is created through the ordinary path on purpose. A warm snapshot is
// only worth having if it holds a guest indistinguishable from one a create would
// have produced, and the readiness gate is the thing that establishes that: an
// agent that answered a health check is the definition of "booted" this platform
// uses everywhere else. Constructing a cheaper almost-boot here would produce a
// snapshot whose guests differ from cold ones in ways nothing would report.
func (m *Manager) warmSnapshotFor(ctx context.Context, imageRef string) error {
	warmer, ok := m.rt.(runtime.SnapshotWarmer)
	if !ok || !warmer.WarmEnabled() {
		return nil
	}
	// Asked before doing any work: an image with no digest cannot be warmed, and
	// booting a guest only to discover that would cost the 5 CPU-seconds this is
	// meant to save, once per prewarm.
	if _, canWarm, err := warmer.WarmKeyFor(imageRef); err != nil || !canWarm {
		if err == nil {
			// Said out loud, because silence here is indistinguishable from success
			// and the consequence is that every create of this image boots forever.
			// An image reaches this state by having no manifest -- a build's output or
			// a commit -- or by having been converted before digests were recorded,
			// and only the second is fixable, by re-converting.
			slog.Info("image has no digest, so it cannot have a warm snapshot; "+
				"creates from it will boot", logging.KeyImage, imageRef)
		}
		return err
	}
	if _, release, exists := warmer.WarmLookup(imageRef); exists {
		release()
		return nil
	}

	// Its own id, marked, so an operator seeing it in a listing knows why a sandbox
	// exists that nobody asked for, and so it cannot collide with a user's. Generated
	// here rather than through the control plane's store.NewID because the node must
	// not import the control plane -- this sandbox exists only on this node and is
	// never recorded upstream.
	id, err := newWarmSandboxID()
	if err != nil {
		return err
	}
	spec := &nodev1.SandboxSpec{
		SandboxId: id,
		Image:     imageRef,
		// Deliberately not the requesting sandbox's shape. This guest is discarded
		// after being captured, and Firecracker's machine config is part of the
		// snapshot, so the figures here are what every restore from it starts with.
		// One vCPU and a small ceiling keep a prewarm from taking a whole node's
		// worth of capacity while it runs.
		Cpu:       1,
		MemoryMib: warmSnapshotMemoryMiB,
	}

	start := time.Now()
	if _, err := m.Create(ctx, spec); err != nil {
		return fmt.Errorf("boot a guest to capture: %w", err)
	}
	// Destroyed whether or not the capture worked. A warm sandbox left running would
	// hold a node's memory for a guest nobody can reach, and it is not in the control
	// plane's expected set, so reconciliation would eventually destroy it anyway --
	// after it had been counted against capacity for a while.
	defer func() {
		if derr := m.Destroy(context.WithoutCancel(ctx), id, true); derr != nil {
			slog.Error("cannot destroy the guest booted for a warm snapshot; it holds "+
				"capacity until reconciliation removes it",
				logging.KeySandbox, id, logging.KeyError, derr)
		}
	}()

	if err := warmer.WarmStore(ctx, imageRef, id); err != nil {
		return err
	}
	m.observePhase(ctx, "warm_snapshot", time.Since(start))
	return nil
}

// createOrRestoreWarm boots a guest, or restores this node's warm snapshot for the
// image if it has one.
//
// A hit skips the kernel boot, which is the whole point: measured, a boot costs
// about 5 CPU-seconds of host CPU and a restore costs almost none, so throughput is
// bounded by the boot until it is removed rather than made faster.
//
// A miss boots, and so does every failure. That asymmetry is deliberate and is the
// property that keeps this from being a liability: a corrupt bundle, an unreadable
// one, an image with no digest, or a node that has never warmed anything all take
// the path the node took before warm snapshots existed. The failure mode being
// avoided is one bad warm snapshot making an image unusable across a cluster.
func (m *Manager) createOrRestoreWarm(ctx context.Context, rspec *runtime.Spec,
	imageRef string) (*runtime.Handle, error) {

	warmer, ok := m.rt.(runtime.SnapshotWarmer)
	if !ok || !warmer.WarmEnabled() || imageRef == "" {
		return m.rt.Create(ctx, rspec)
	}
	// A restore of a snapshot the caller named must not be diverted to a warm one:
	// the caller asked for specific state, and this optimisation is only ever a
	// substitute for booting fresh.
	if rspec.SnapshotID != "" {
		return m.rt.Create(ctx, rspec)
	}

	layer, release, hit := warmer.WarmLookup(imageRef)
	if !hit {
		m.metrics.IncCounter("bean_node_warm_lookups_total",
			"Warm snapshot lookups on create.",
			map[string]string{"outcome": "miss", "runtime": m.rt.Name()}, 1)
		return m.rt.Create(ctx, rspec)
	}
	defer release()

	// The spec's snapshot id is set so the runtime caches the unpacked form under
	// the warm key. That is what makes the second and every later warm-started create
	// on this node skip the unpacking as well as the boot.
	warmSpec := *rspec
	warmSpec.SnapshotID = layer.ID

	// This is a *restore*, not a fork, in the vocabulary of
	// docs/snapshot-resume.md section 0: it starts from a bundle on disk, produces a
	// new sandbox with a new id, and survives a noded restart. A fork starts from a
	// running sandbox and leaves no persistent object; the guest this bundle was
	// captured from was destroyed when the prewarm finished.
	//
	// The method is named Fork because FCRuntime routes Create and Fork through one
	// create(spec, layers) and a non-empty layers means "start from a checkpoint".
	// The name is the runtime's existing inconsistency rather than a claim about
	// which operation this is.
	handle, err := m.rt.Fork(ctx, &warmSpec, []runtime.SnapshotLayer{layer})
	if err != nil {
		// Fall back rather than fail. The bundle was present and readable and still
		// did not restore, which means it is unusable and every create of this image
		// would fail the same way -- so the honest response is to boot and say so.
		slog.Warn("warm snapshot did not restore; booting instead",
			logging.KeyImage, imageRef, logging.KeySnapshot, layer.ID,
			logging.KeyError, err)
		m.metrics.IncCounter("bean_node_warm_lookups_total",
			"Warm snapshot lookups on create.",
			map[string]string{"outcome": "restore_failed", "runtime": m.rt.Name()}, 1)
		return m.rt.Create(ctx, rspec)
	}
	m.metrics.IncCounter("bean_node_warm_lookups_total",
		"Warm snapshot lookups on create.",
		map[string]string{"outcome": "hit", "runtime": m.rt.Name()}, 1)
	return handle, nil
}

// newWarmSandboxID names the throwaway guest a warm capture boots.
//
// Random rather than derived from the image, because two prewarms of the same image
// must not pick the same id: Create rejects a duplicate, so a derived id would make
// a concurrent prewarm fail instead of being harmlessly redundant.
func newWarmSandboxID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate warm sandbox id: %w", err)
	}
	return "warmsbx_" + hex.EncodeToString(b), nil
}

// warmSnapshotMemoryMiB is the guest size a warm snapshot is captured at.
//
// It bounds every sandbox restored from it, because Firecracker's machine config
// travels inside the snapshot and a restore cannot raise it. So this is not a
// prewarm-time convenience but the memory ceiling of every warm-started sandbox,
// which is why it is a named constant with a stated basis rather than a literal:
// 512 MiB is what the existing create path defaults to, so a warm-started sandbox
// gets what a cold one would.
const warmSnapshotMemoryMiB = 512

// redialAgent replaces a sandbox's agent connection.
func (m *Manager) redialAgent(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	old, handle := sb.conn, sb.Handle
	m.mu.Unlock()

	conn, err := m.connectAgent(ctx, handle)
	if err != nil {
		return err
	}

	m.mu.Lock()
	sb, ok = m.sandboxes[id]
	if !ok {
		// Destroyed while dialling: drop the new connection rather than
		// attaching it to a record that no longer exists.
		m.mu.Unlock()
		conn.Close()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	sb.conn = conn
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}
	return nil
}

// ForkSandbox creates a sandbox from a checkpoint chain, ordered base-first. It
// mirrors Create, including agent health-checking, so the new sandbox is
// immediately usable through the same data-plane paths.
//
// A self-contained checkpoint is one layer; an incremental one is its whole
// chain, since a diff holds only what changed since its base.
//
// This may be called many times, concurrently, against one checkpoint: the
// runtime shares the read-only parts and copies the writable ones, so each call
// yields an independent sandbox rather than competing to recover the same one.
// The spec's sandbox id is what distinguishes them, and a duplicate is rejected.
func (m *Manager) ForkSandbox(ctx context.Context, spec *nodev1.SandboxSpec,
	layers []runtime.SnapshotLayer) (*Sandbox, error) {
	if spec.GetSandboxId() == "" {
		return nil, fmt.Errorf("sandbox_id required")
	}
	// Span and metric names are unchanged: they are the operational interface that
	// dashboards and alerts are written against, and renaming them would make
	// existing history look like a gap rather than a rename.
	ctx, span := obs.Tracer("noded").Start(ctx, "node.Restore",
		trace.WithAttributes(
			attribute.String(obs.AttrSandbox, spec.GetSandboxId()),
			attribute.String(obs.AttrSnapshot, spec.GetSnapshotId()),
		))
	defer span.End()

	m.mu.Lock()
	if _, exists := m.sandboxes[spec.SandboxId]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s already exists", spec.SandboxId)
	}
	sb := &Sandbox{Spec: spec, State: runtime.StateRestoring, lastActivity: time.Now()}
	m.sandboxes[spec.SandboxId] = sb
	m.mu.Unlock()

	start := time.Now()
	outcome := "error"
	defer func() {
		m.metrics.IncCounter("bean_node_restores_total",
			"Sandbox restores handled by this node.",
			map[string]string{"outcome": outcome, "runtime": m.rt.Name()}, 1)
		m.observePhase(ctx, "restore", time.Since(start))
	}()

	rspec := specToRuntime(spec)

	// A fork needs its own namespace for the same reason a cold start does, and
	// more sharply: this is the fan-out case docs/network.md is built around. N
	// sandboxes derived from one checkpoint all come back holding the identical
	// guest address, and what keeps that from colliding is each one sitting in a
	// namespace of its own. Skipping this would leave a restored guest looking for
	// beantap0 in the host namespace, where either nothing answers or -- worse --
	// another sandbox's tap does.
	if m.Net != nil {
		layout, err := m.Net.Provision(spec.SandboxId)
		if err != nil {
			m.dropFailed(spec.SandboxId)
			return nil, fmt.Errorf("sandbox %s: %w", spec.SandboxId, err)
		}
		rspec.Network = layout

		// A fresh token, not the source's. This is the reason /mmds is rewritten after
		// a restore rather than carried in the snapshot: a fork is a different sandbox
		// with different contents and possibly a different owner, and letting it accept
		// its ancestor's credential would make one leaked token good for every sandbox
		// descended from that checkpoint.
		token, terr := sbxtoken.New()
		if terr != nil {
			m.dropFailed(spec.SandboxId)
			return nil, fmt.Errorf("sandbox %s: mint agent token: %w", spec.SandboxId, terr)
		}
		rspec.AgentTokenHash = sbxtoken.Hash(token)
		m.mu.Lock()
		if cur, ok := m.sandboxes[spec.SandboxId]; ok {
			cur.agentToken = token
			cur.net = layout
		}
		m.mu.Unlock()
	}

	// Unpacking the bundle and loading it into a VMM are measured apart from
	// waiting for the agent: the first scales with snapshot size and the second
	// does not, so a single number for the whole restore cannot say which one to
	// attack.
	loadStart := time.Now()
	rCtx, rSpan := obs.Tracer("noded").Start(ctx, "runtime.Restore",
		trace.WithAttributes(obs.Phase("restore_load"),
			attribute.String(obs.AttrSnapshot, spec.GetSnapshotId())))
	handle, err := m.rt.Fork(rCtx, rspec, layers)
	obs.Fail(rCtx, err)
	rSpan.End()
	m.observePhase(ctx, "restore_load", time.Since(loadStart))
	if err != nil {
		m.dropFailed(spec.SandboxId)
		return nil, err
	}
	conn, err := m.dialAgent(ctx, spec.SandboxId, handle)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	sb.Handle = handle
	sb.conn = conn
	sb.State = runtime.StateRunning
	m.mu.Unlock()
	outcome = "success"
	return sb, nil
}

// dialAgent connects to a sandbox's agent and waits for it to be healthy,
// cleaning up the sandbox if it never comes up.
func (m *Manager) dialAgent(ctx context.Context, id string, handle *runtime.Handle) (*grpc.ClientConn, error) {
	conn, err := m.connectAgent(ctx, handle)
	if err != nil {
		// A sandbox whose agent never answers is not usable, so creation tears
		// it down rather than leaving a record pointing at nothing.
		_ = m.rt.Destroy(context.Background(), id, true)
		m.dropFailed(id)
		return nil, err
	}
	return conn, nil
}

// connectAgent dials a sandbox's agent and waits for it to answer, without
// touching the sandbox record. Reconnecting to a healthy sandbox must not
// destroy it on failure, which is why this is separate from dialAgent.
func (m *Manager) connectAgent(ctx context.Context, handle *runtime.Handle) (*grpc.ClientConn, error) {
	// The credential is read per call rather than captured now, because a restore
	// mints a new one for an existing record: a value bound at dial time would be
	// the pre-fork token, and the agent would reject every call on a resumed
	// sandbox. Reading through the record means the connection always presents
	// whatever the guest was last told to expect.
	//
	// An interceptor rather than each call site remembering. There are six
	// data-plane methods plus health checks, and a credential that has to be
	// attached by hand is one that will be missing from whichever call is added
	// next -- and the symptom would be a permission error blamed on the agent.
	tokenFor := func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		if sb, ok := m.sandboxes[handle.SandboxID]; ok {
			return sb.agentToken
		}
		return ""
	}
	// The address is handed to the dialer verbatim rather than parsed by gRPC:
	// a vsock target carries a socket path and a port, which gRPC's name
	// resolution rejects as "too many colons". passthrough turns off that
	// parsing, so the runtime tier owns the address format.
	conn, err := grpc.NewClient("passthrough:///"+handle.AgentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// A microVM agent is reachable over vsock rather than a socket path,
		// so the transport depends on the runtime tier while everything above
		// this line does not.
		grpc.WithContextDialer(dialAgentAddr),
		// The agent does not listen until the guest has booted, so the first
		// dial always fails. gRPC's default backoff then waits a second before
		// retrying, which put a floor under create latency far above the boot
		// itself: the guest was ready at ~700ms but the connection stayed parked
		// until the backoff expired. The retry interval has to be on the
		// timescale of a boot, not of a remote service outage.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  20 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   time.Second,
			},
			MinConnectTimeout: 2 * time.Second,
		}),
		// The agent runs inside the guest and cannot reach a collector itself
		// (it has no route out, only an inbound vsock), so it does not export
		// spans. Injecting the trace context anyway means the agent's own logs
		// carry the same trace id as the surrounding spans, which is what makes
		// "the slow part was inside the guest" a followable claim.
		grpc.WithChainUnaryInterceptor(obs.UnaryClientTrace(),
			func(ctx context.Context, method string, req, reply any,
				cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
				opts ...grpc.CallOption) error {
				return invoker(sbxtoken.WithAgentToken(ctx, tokenFor()), method, req,
					reply, cc, opts...)
			}),
		grpc.WithChainStreamInterceptor(obs.StreamClientTrace(),
			func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
				method string, streamer grpc.Streamer,
				opts ...grpc.CallOption) (grpc.ClientStream, error) {
				return streamer(sbxtoken.WithAgentToken(ctx, tokenFor()), desc, cc,
					method, opts...)
			}))
	if err != nil {
		return nil, fmt.Errorf("agent dial: %w", err)
	}
	healthStart := time.Now()
	// A microVM has to boot a kernel and pivot to the user image before its
	// agent listens, which takes longer than a process-level sandbox needs.
	// This span covers guest boot: it is normally the largest single piece of
	// a create, and the one a reader most often wants isolated from the rest.
	hCtx, hSpan := obs.Tracer("noded").Start(ctx, "agent.WaitHealthy",
		trace.WithAttributes(obs.Phase("agent_ready")))
	err = waitHealthy(hCtx, conn, agentReadyTimeout)
	obs.Fail(hCtx, err)
	hSpan.End()
	m.observePhase(ctx, "agent_ready", time.Since(healthStart))
	if err != nil {
		conn.Close()
		// A timeout here names the symptom and no cause. Every way a guest can fail
		// to finish booting produces this same error -- a kernel that found no root
		// device, an agent that rejected its own arguments, a misconfigured vsock, or
		// a sandbox that is merely slow -- and the evidence that separates them is in
		// the guest console, which the cleanup below is about to delete.
		// 40 lines rather than 6. Measured: an overlaybd guest failed with
		// "Kernel panic - not syncing: Attempted to kill init! exitcode=0x00000100"
		// and 6 lines held the panic and nothing else -- the agent's own error, which
		// is the line that says *why* init exited, had already scrolled past. A tail
		// that shows the symptom and truncates the cause is the failure this whole
		// block was added to prevent.
		//
		// The cost of the larger window is a longer error string on a path that has
		// already failed, which is the cheapest place in the system to spend bytes.
		if d, ok := m.rt.(runtime.BootDiagnoser); ok {
			if tail := d.BootLogTail(handle.SandboxID, 40); tail != "" {
				return nil, fmt.Errorf("agent health: %w (guest console: %s)", err, tail)
			}
		}
		return nil, fmt.Errorf("agent health: %w", err)
	}
	return conn, nil
}

// agentReadyTimeout bounds how long a sandbox has to bring its agent up.
// Measured microVM boot to a healthy agent is around two seconds, so this
// leaves room for a loaded node without waiting so long that a genuinely
// broken sandbox ties up a create.
const agentReadyTimeout = 20 * time.Second

// guestFlushCommand is what the guest runs to make its filesystem durable before a
// memory-less checkpoint reads the block device.
//
// Named rather than inline because it was changed to `sync -f /` (syncfs on the mount) on the
// theory that plain `sync` does not order a file's data block against the inode referencing it,
// and that this explained a restore reading an empty file. Measured on hardware, syncfs was
// *worse*: 8/25 and 7/16 against 8/10 for plain `sync`. So the ordering theory does not hold, or
// does not hold alone, and the constant stays plain `sync` until something is measured to beat it.
//
// About 20% of memory-less snapshots on the ublk route still restore without the most recent
// write. docs/status.md records what has been ruled out.
const guestFlushCommand = "sync"

// guestSyncTimeout bounds a flush of the guest's page cache.
//
// A sync of a sandbox's own writable layer is milliseconds. The bound is short
// because both callers sit on paths whose whole point was to stop waiting
// seconds on a guest that may never answer: teardown, and a snapshot taken
// without memory.
const guestSyncTimeout = 2 * time.Second

// specToRuntime projects the proto spec onto the runtime's view.
func specToRuntime(spec *nodev1.SandboxSpec) *runtime.Spec {
	return &runtime.Spec{
		SandboxID:         spec.SandboxId,
		SnapshotID:        spec.SnapshotId,
		FSManifestDigest:  spec.FsManifestDigest,
		Image:             spec.Image,
		PublishConversion: spec.PublishConversion,
		CPU:               spec.Cpu,
		MemoryMiB:         spec.MemoryMib,
		DiskMiB:           spec.DiskMib,
		Env:               spec.Env,
		Cmd:               spec.Cmd,
		AutoStartCmd:      spec.AutoStartCmd,
	}
}

func (m *Manager) Resume(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	switch sb.State {
	case runtime.StateRunning:
		m.mu.Unlock()
		return nil // idempotent
	case runtime.StatePaused:
		sb.State = runtime.StateResuming
		m.mu.Unlock()
	default:
		state := sb.State
		m.mu.Unlock()
		return fmt.Errorf("sandbox %s not PAUSED (state=%s)", id, state)
	}

	if err := m.rt.Resume(ctx, id); err != nil {
		m.mu.Lock()
		if cur, ok := m.sandboxes[id]; ok && cur.State == runtime.StateResuming {
			cur.State = runtime.StatePaused
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	if cur, ok := m.sandboxes[id]; ok {
		cur.State = runtime.StateRunning
		cur.lastActivity = time.Now()
	}
	m.mu.Unlock()
	m.kick()
	return nil
}

// Statuses lists all sandboxes for heartbeat/reconcile.
func (m *Manager) Statuses() []*nodev1.SandboxStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*nodev1.SandboxStatus, 0, len(m.sandboxes))
	for id, sb := range m.sandboxes {
		st := &nodev1.SandboxStatus{
			SandboxId:        id,
			State:            string(sb.State),
			Reason:           sb.Reason,
			LastActivityUnix: sb.lastActivity.Unix(),
		}
		if sb.Handle != nil {
			st.StartedAtUnix = sb.Handle.StartedAt.Unix()
		}
		out = append(out, st)
	}
	return out
}

// idleLoop enforces lifecycle.idleTimeout/onIdle locally (no control-plane dependency).
func (m *Manager) idleLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sweepIdle()
		}
	}
}

func (m *Manager) sweepIdle() {
	type action struct {
		id     string
		onIdle string
	}
	var acts []action
	now := time.Now()
	m.mu.Lock()
	for id, sb := range m.sandboxes {
		lc := sb.Spec.GetLifecycle()
		if lc == nil || !lc.HasIdleTimeout || sb.State != runtime.StateRunning {
			continue
		}
		if sb.inFlight > 0 {
			continue // never freeze/kill a sandbox with a request in progress
		}
		idle := now.Sub(sb.lastActivity)
		if idle >= time.Duration(lc.IdleTimeoutSeconds)*time.Second {
			acts = append(acts, action{id: id, onIdle: lc.OnIdle})
		}
	}
	m.mu.Unlock()

	for _, a := range acts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		switch a.onIdle {
		case "delete":
			err = m.Destroy(ctx, a.id, false)
		default: // pause
			err = m.Pause(ctx, a.id)
		}
		cancel()
		m.metrics.IncCounter("bean_node_idle_actions_total",
			"Sandboxes acted on by the idle sweep.",
			map[string]string{"action": a.onIdle, "outcome": boolOutcome(err == nil)}, 1)
		if err != nil {
			slog.Error("idle sweep failed", logging.KeySandbox, a.id, "action", a.onIdle, logging.KeyError, err)
		} else {
			slog.Info("sandbox idled out", logging.KeySandbox, a.id, "action", a.onIdle)
		}
	}
}

// RefreshGauges recomputes node-level gauges; called at scrape time so the
// numbers are authoritative rather than incrementally maintained.
func (m *Manager) RefreshGauges() {
	m.mu.Lock()
	counts := map[string]float64{}
	var inFlight float64
	for _, sb := range m.sandboxes {
		counts[string(sb.State)]++
		inFlight += float64(sb.inFlight)
	}
	m.mu.Unlock()

	for _, st := range []runtime.State{
		runtime.StateStarting, runtime.StateRunning, runtime.StatePausing,
		runtime.StatePaused, runtime.StateResuming, runtime.StateFailed,
	} {
		if _, ok := counts[string(st)]; !ok {
			counts[string(st)] = 0
		}
	}
	for st, n := range counts {
		m.metrics.SetGauge("bean_node_sandboxes", "Sandboxes on this node by state.",
			map[string]string{"state": st}, n)
	}
	m.metrics.SetGauge("bean_node_requests_in_flight",
		"Data-plane requests currently in flight across sandboxes.", nil, inFlight)

	// Reported because this space is not committed to anything, so it is the one
	// part of a node's disk that can grow without the scheduler noticing. A
	// measured 4.6 GB accumulated on a development node before it was bounded.
	if reporter, ok := m.rt.(runtime.CacheReporter); ok {
		if used, err := reporter.SnapshotCacheBytes(); err == nil {
			m.metrics.SetGauge("bean_node_snapshot_cache_bytes",
				"Disk held by unpacked snapshots, which no commitment covers.",
				nil, float64(used))
		}
	}

	// Actual occupancy, alongside the commitment the scheduler tracks. The two
	// disagree by orders of magnitude — a 20 GiB sparse layer holding 44 KiB is
	// counted as 20 GiB by the ledger — and the gap is only visible if both are
	// published.
	if m.Disk.Path != "" {
		if stats, err := m.Disk.Stat(); err == nil {
			m.metrics.SetGauge("bean_node_disk_free_bytes",
				"Free space on the sandbox filesystem, excluding the root reserve.",
				nil, float64(stats.FreeBytes))
			m.metrics.SetGauge("bean_node_disk_used_bytes",
				"Allocated blocks on the sandbox filesystem, which is what sparse "+
					"layers actually cost.", nil, float64(stats.UsedBytes))
		}
	}
}

// DiskUsedMiB reports the sandbox filesystem's real occupancy for the heartbeat,
// in MiB to match the commitment fields it sits beside.
//
// Zero when the guard has no path configured, which reads as "not reported"
// rather than "empty": the control plane treats it as advisory, so a node that
// cannot measure itself is not mistaken for one with an empty disk.
func (m *Manager) DiskUsedMiB() int64 {
	if m.Disk.Path == "" {
		return 0
	}
	stats, err := m.Disk.Stat()
	if err != nil {
		return 0
	}
	return stats.UsedBytes >> 20
}
