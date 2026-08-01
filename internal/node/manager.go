// Package node implements noded: sandbox lifecycle management on one node.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/obs"
)

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
func (m *Manager) observePhase(phase string, d time.Duration) {
	m.metrics.ObserveDuration("bean_node_create_phase_seconds",
		"Sandbox create latency by phase.",
		map[string]string{"phase": phase, "runtime": m.rt.Name()}, d)
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
	m.mu.Lock()
	if _, exists := m.sandboxes[spec.SandboxId]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s already exists", spec.SandboxId)
	}
	// Reserve the slot to make Create idempotent-safe under concurrency.
	sb := &Sandbox{Spec: spec, State: runtime.StateStarting, lastActivity: time.Now()}
	m.sandboxes[spec.SandboxId] = sb
	m.mu.Unlock()

	rspec := &runtime.Spec{
		SandboxID:    spec.SandboxId,
		Image:        spec.Image,
		CPU:          spec.Cpu,
		MemoryMiB:    spec.MemoryMib,
		DiskMiB:      spec.DiskMib,
		Env:          spec.Env,
		Cmd:          spec.Cmd,
		AutoStartCmd: spec.AutoStartCmd,
	}

	createStart := time.Now()
	outcome := "error"
	defer func() {
		m.metrics.IncCounter("bean_node_creates_total",
			"Sandbox creates handled by this node.",
			map[string]string{"outcome": outcome, "runtime": m.rt.Name()}, 1)
		m.observePhase("total", time.Since(createStart))
	}()

	rtStart := time.Now()
	handle, err := m.rt.Create(ctx, rspec)
	m.observePhase("runtime_create", time.Since(rtStart))
	if err != nil {
		m.dropFailed(spec.SandboxId)
		return nil, err
	}

	conn, err := grpc.NewClient(handle.AgentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = m.rt.Destroy(context.Background(), spec.SandboxId, true)
		m.dropFailed(spec.SandboxId)
		return nil, fmt.Errorf("agent dial: %w", err)
	}
	healthStart := time.Now()
	err = waitHealthy(ctx, conn, 5*time.Second)
	m.observePhase("agent_ready", time.Since(healthStart))
	if err != nil {
		conn.Close()
		_ = m.rt.Destroy(context.Background(), spec.SandboxId, true)
		m.dropFailed(spec.SandboxId)
		return nil, fmt.Errorf("agent health: %w", err)
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
				log.Printf("sandbox %s: autoStartCmd failed: %v", spec.SandboxId, err)
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
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dropFailed removes a sandbox whose creation failed. The runtime has
// already been cleaned up, so keeping a FAILED entry would leak memory
// with no way to reclaim it.
func (m *Manager) dropFailed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sandboxes, id)
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
	err := m.rt.Destroy(ctx, id, force)
	m.observePhase("destroy", time.Since(start))
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
			log.Printf("idle sweep %s (%s): %v", a.id, a.onIdle, err)
		} else {
			log.Printf("idle sweep: sandbox %s -> %s", a.id, a.onIdle)
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
}
