package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bean-agent-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "bean-agent")
	cmd := exec.Command("go", "build", "-o", agentBin, "github.com/garysng/bean/cmd/bean-agent")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build agent: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	rt := runtime.NewLocalRuntime(agentBin, t.TempDir())
	m := NewManager(rt)
	t.Cleanup(m.Close)
	return m
}

func spec(id string, mut ...func(*nodev1.SandboxSpec)) *nodev1.SandboxSpec {
	s := &nodev1.SandboxSpec{
		SandboxId: id,
		Image:     "test:latest",
		Cpu:       1,
		MemoryMib: 256,
	}
	for _, f := range mut {
		f(s)
	}
	return s
}

func TestCreateDestroy(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	sb, err := m.Create(ctx, spec("sbx-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != runtime.StateRunning {
		t.Errorf("state = %s, want RUNNING", sb.State)
	}
	if _, err := m.AgentConn(ctx, "sbx-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(ctx, "sbx-1", false); err != nil {
		t.Fatal(err)
	}
	if m.Get("sbx-1") != nil {
		t.Error("sandbox still present after destroy")
	}
	// idempotent destroy
	if err := m.Destroy(ctx, "sbx-1", true); err != nil {
		t.Errorf("second destroy: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, spec("dup")); err == nil {
		t.Error("expected duplicate error")
	}
}

func TestPauseResumeAndWake(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("p1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if got := m.StateOf("p1"); got != runtime.StatePaused {
		t.Fatalf("state = %s", got)
	}
	// pause of paused rejected
	if err := m.Pause(ctx, "p1"); err == nil {
		t.Error("expected error pausing PAUSED sandbox")
	}
	// AgentConn transparently wakes
	if _, err := m.AgentConn(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if got := m.StateOf("p1"); got != runtime.StateRunning {
		t.Errorf("state after wake = %s, want RUNNING", got)
	}
	// resume of running is idempotent
	if err := m.Resume(ctx, "p1"); err != nil {
		t.Errorf("resume running: %v", err)
	}
}

func TestIdleSweepKill(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	sp := spec("idle-kill", func(s *nodev1.SandboxSpec) {
		s.Lifecycle = &nodev1.Lifecycle{HasIdleTimeout: true, IdleTimeoutSeconds: 1, OnIdle: "kill"}
	})
	if _, err := m.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Get("idle-kill") != nil {
		if time.Now().After(deadline) {
			t.Fatal("idle sweep did not kill sandbox")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestIdleSweepPause(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	sp := spec("idle-pause", func(s *nodev1.SandboxSpec) {
		s.Lifecycle = &nodev1.Lifecycle{HasIdleTimeout: true, IdleTimeoutSeconds: 1, OnIdle: "pause"}
	})
	if _, err := m.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := m.StateOf("idle-pause")
		if st == "" {
			t.Fatal("sandbox disappeared")
		}
		if st == runtime.StatePaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle sweep did not pause sandbox")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// wake via AgentConn resets activity; sandbox back to RUNNING
	if _, err := m.AgentConn(ctx, "idle-pause"); err != nil {
		t.Fatal(err)
	}
	if got := m.StateOf("idle-pause"); got != runtime.StateRunning {
		t.Errorf("state = %s", got)
	}
}

func TestStatuses(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("s1")); err != nil {
		t.Fatal(err)
	}
	sts := m.Statuses()
	if len(sts) != 1 || sts[0].SandboxId != "s1" || sts[0].State != "RUNNING" {
		t.Errorf("statuses = %+v", sts)
	}
}
