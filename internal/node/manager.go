// Package node implements noded: sandbox lifecycle management on one node.
package node

import (
	"context"
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
	"github.com/garysng/bean/internal/node/network"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/obs"
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
}

func NewManager(rt runtime.Runtime) *Manager {
	m := &Manager{
		rt:        rt,
		metrics:   obs.NewRegistry(),
		sandboxes: map[string]*Sandbox{},
		stopCh:    make(chan struct{}),
	}
	go m.idleLoop()
	return m
}

// Metrics exposes the node's registry for the /metrics endpoint.
func (m *Manager) Metrics() *obs.Registry { return m.metrics }

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
func (m *Manager) Create(ctx context.Context, spec *nodev1.SandboxSpec) (*Sandbox, error) {
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
	sb := &Sandbox{Spec: spec, State: runtime.StateStarting, lastActivity: time.Now()}
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
	}

	rtStart := time.Now()
	rtCtx, rtSpan := obs.Tracer("noded").Start(ctx, "runtime.Create",
		trace.WithAttributes(obs.Phase("runtime_create")))
	handle, err := m.rt.Create(rtCtx, rspec)
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

	if spec.AutoStartCmd {
		// Fire-and-forget: replay of the image entrypoint. In localRuntime
		// dev mode the image config is not resolved; spec.Cmd is used.
		if len(spec.Cmd) > 0 {
			_, err := agentv1.NewAgentServiceClient(conn).StartUserProcess(ctx,
				&agentv1.StartUserProcessRequest{Cmd: spec.Cmd, Env: spec.Env})
			if err != nil {
				slog.Error("autoStartCmd failed", logging.KeySandbox, spec.SandboxId, logging.KeyError, err)
			}
		}
	}
	return sb, nil
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
	if !force {
		if err := m.flushBeforeDestroy(ctx, id); err != nil {
			// A sandbox being destroyed cannot be kept alive because its
			// filesystem would not flush, so this is reported and not fatal.
			logging.From(ctx).Warn("flush before destroy failed",
				logging.KeySandbox, id, logging.KeyError, err)
		}
	}

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
	m.releaseNetwork(id)

	m.metrics.IncCounter("bean_node_destroys_total", "Sandbox destroys handled by this node.",
		map[string]string{"outcome": boolOutcome(err == nil), "runtime": m.rt.Name()}, 1)
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
	opts runtime.CheckpointOptions) error {
	// Claim the transition under lock, remembering where to go back to.
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	prev := sb.State
	if prev != runtime.StateRunning && prev != runtime.StatePaused {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %s is %s; snapshot needs RUNNING or PAUSED", id, prev)
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
	//
	// This is the same reason CommitSandbox syncs, and for the same tier.
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
			return fmt.Errorf("freeze for snapshot: %w", err)
		}
	}

	start := time.Now()
	err := m.rt.Checkpoint(ctx, id, w, opts)
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
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// CommitSandbox turns a sandbox's filesystem into a base image.
//
// The sandbox is frozen for the read and returned to its prior state
// afterwards, the same discipline Snapshot uses: a filesystem read while
// processes are writing to it would capture a torn state that fails to mount.
func (m *Manager) CommitSandbox(ctx context.Context, id, tag string) error {
	committer, ok := m.rt.(runtime.SandboxCommitter)
	if !ok {
		return fmt.Errorf("runtime %s cannot commit sandboxes", m.rt.Name())
	}

	// The guest has to flush before its filesystem is read from the host.
	// Pausing stops the vCPUs but leaves the guest's page cache dirty, and a
	// commit reads the block device — so without this the image silently lacks
	// whatever the sandbox wrote most recently, which is exactly the work the
	// user is trying to keep. (A snapshot needs no such step: Firecracker
	// captures guest memory, so the dirty pages travel with it.)
	//
	// This runs before the state transition is claimed, because flushing goes
	// through the agent and the data plane only serves runnable sandboxes.
	if m.StateOf(id) == runtime.StateRunning {
		if err := m.syncGuest(ctx, id); err != nil {
			return fmt.Errorf("flush guest filesystem: %w", err)
		}
	}

	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	prev := sb.State
	if prev != runtime.StateRunning && prev != runtime.StatePaused {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %s is %s; commit needs RUNNING or PAUSED", id, prev)
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

	if prev == runtime.StateRunning {
		if err := m.rt.Pause(ctx, id); err != nil {
			restore()
			return fmt.Errorf("freeze for commit: %w", err)
		}
	}

	start := time.Now()
	err := committer.CommitSandbox(ctx, id, tag)
	m.observePhase(ctx, "commit", time.Since(start))
	m.metrics.IncCounter("bean_node_commits_total",
		"Sandbox commits on this node.",
		map[string]string{"outcome": boolOutcome(err == nil), "runtime": m.rt.Name()}, 1)

	if prev == runtime.StateRunning {
		if rerr := m.rt.Resume(ctx, id); rerr != nil {
			slog.Error("resume after commit failed", logging.KeySandbox, id, logging.KeyError, rerr)
		}
	}
	restore()
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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

// BuildImage builds a base image on this node.
//
// Cancelling ctx stops the build: the runtime runs its builder under it, so the
// context is the only handle a caller needs. That is also why the outcome label
// distinguishes a cancelled build from a failed one — a rate of builds someone
// stopped on purpose says nothing about whether this node's BuildKit is healthy,
// and counting the two together is how a broken builder hides.
func (m *Manager) BuildImage(ctx context.Context, req runtime.BuildRequest) (string, error) {
	builder, ok := m.rt.(runtime.ImageBuilder)
	if !ok {
		return "", fmt.Errorf("runtime %s cannot build images", m.rt.Name())
	}
	start := time.Now()
	ref, err := builder.BuildImage(ctx, req)
	m.observePhase(ctx, "image_build", time.Since(start))
	m.metrics.IncCounter("bean_node_image_builds_total",
		"Image builds on this node.",
		map[string]string{"outcome": buildOutcome(ctx, err), "runtime": m.rt.Name()}, 1)
	return ref, err
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

// CachedImages reports the images this node holds, for the heartbeat.
//
// A runtime with no image cache returns nothing rather than an error: the local
// tier runs a host binary, so there is genuinely nothing cached, and a heartbeat
// should not fail over it.
func (m *Manager) CachedImages() map[string]int64 {
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
	return err
}

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
		grpc.WithChainUnaryInterceptor(obs.UnaryClientTrace()),
		grpc.WithChainStreamInterceptor(obs.StreamClientTrace()))
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
		return nil, fmt.Errorf("agent health: %w", err)
	}
	return conn, nil
}

// agentReadyTimeout bounds how long a sandbox has to bring its agent up.
// Measured microVM boot to a healthy agent is around two seconds, so this
// leaves room for a loaded node without waiting so long that a genuinely
// broken sandbox ties up a create.
const agentReadyTimeout = 20 * time.Second

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
		SandboxID:    spec.SandboxId,
		SnapshotID:   spec.SnapshotId,
		Image:        spec.Image,
		CPU:          spec.Cpu,
		MemoryMiB:    spec.MemoryMib,
		DiskMiB:      spec.DiskMib,
		Env:          spec.Env,
		Cmd:          spec.Cmd,
		AutoStartCmd: spec.AutoStartCmd,
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
		case "kill":
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
