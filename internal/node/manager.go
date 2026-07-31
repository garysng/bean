// Package node implements beand: sandbox lifecycle management on one node.
package node

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// Sandbox is beand's in-memory record of one sandbox.
type Sandbox struct {
	Spec   *nodev1.SandboxSpec
	Handle *runtime.Handle
	State  runtime.State
	Reason string

	conn         *grpc.ClientConn
	lastActivity time.Time
}

// Manager owns all sandboxes on this node.
type Manager struct {
	rt runtime.Runtime

	mu        sync.Mutex
	sandboxes map[string]*Sandbox

	stopCh chan struct{}
}

func NewManager(rt runtime.Runtime) *Manager {
	m := &Manager{
		rt:        rt,
		sandboxes: map[string]*Sandbox{},
		stopCh:    make(chan struct{}),
	}
	go m.idleLoop()
	return m
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
	handle, err := m.rt.Create(ctx, rspec)
	if err != nil {
		m.setFailed(spec.SandboxId, err.Error())
		return nil, err
	}

	conn, err := grpc.NewClient(handle.AgentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = m.rt.Destroy(context.Background(), spec.SandboxId, true)
		m.setFailed(spec.SandboxId, "agent dial: "+err.Error())
		return nil, err
	}
	if err := waitHealthy(ctx, conn, 5*time.Second); err != nil {
		conn.Close()
		_ = m.rt.Destroy(context.Background(), spec.SandboxId, true)
		m.setFailed(spec.SandboxId, "agent health: "+err.Error())
		return nil, err
	}

	m.mu.Lock()
	sb.Handle = handle
	sb.conn = conn
	sb.State = runtime.StateRunning
	m.mu.Unlock()

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

func (m *Manager) setFailed(id, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		sb.State = runtime.StateFailed
		sb.Reason = reason
	}
}

// Get returns the sandbox or nil.
func (m *Manager) Get(id string) *Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxes[id]
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

// AgentConn returns a live agent connection for RUNNING sandboxes,
// transparently resuming PAUSED ones (wake-on-request).
func (m *Manager) AgentConn(ctx context.Context, id string) (*grpc.ClientConn, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s not found", id)
	}
	state := sb.State
	conn := sb.conn
	sb.lastActivity = time.Now()
	m.mu.Unlock()

	switch state {
	case runtime.StateRunning:
		return conn, nil
	case runtime.StatePaused:
		if err := m.Resume(ctx, id); err != nil {
			return nil, fmt.Errorf("wake: %w", err)
		}
		return conn, nil
	default:
		return nil, fmt.Errorf("sandbox %s not runnable (state=%s)", id, state)
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
	return m.rt.Destroy(ctx, id, force)
}

func (m *Manager) Pause(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("sandbox %s not found", id)
	}
	if sb.State != runtime.StateRunning {
		return fmt.Errorf("sandbox %s not RUNNING (state=%s)", id, sb.State)
	}
	if err := m.rt.Pause(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	sb.State = runtime.StatePaused
	m.mu.Unlock()
	return nil
}

func (m *Manager) Resume(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("sandbox %s not found", id)
	}
	if sb.State != runtime.StatePaused {
		if sb.State == runtime.StateRunning {
			return nil // idempotent
		}
		return fmt.Errorf("sandbox %s not PAUSED (state=%s)", id, sb.State)
	}
	if err := m.rt.Resume(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	sb.State = runtime.StateRunning
	sb.lastActivity = time.Now()
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
		if err != nil {
			log.Printf("idle sweep %s (%s): %v", a.id, a.onIdle, err)
		} else {
			log.Printf("idle sweep: sandbox %s -> %s", a.id, a.onIdle)
		}
	}
}

// TouchActivity records data-plane activity (exec/files) for idle tracking.
func (m *Manager) TouchActivity(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		sb.lastActivity = time.Now()
	}
}
