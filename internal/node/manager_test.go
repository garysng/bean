package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "beand-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "beand")
	cmd := exec.Command("go", "build", "-o", agentBin, "github.com/garysng/bean/cmd/beand")
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
	if _, rel, err := m.AgentConn(ctx, "sbx-1"); err != nil {
		t.Fatal(err)
	} else {
		rel()
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
	if _, rel, err := m.AgentConn(ctx, "p1"); err != nil {
		t.Fatal(err)
	} else {
		rel()
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
	if _, rel, err := m.AgentConn(ctx, "idle-pause"); err != nil {
		t.Fatal(err)
	} else {
		rel()
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

func TestAgentConnNotFoundIsTyped(t *testing.T) {
	m := newTestManager(t)
	_, _, err := m.AgentConn(context.Background(), "nope")
	if !errors.Is(err, ErrSandboxNotFound) {
		t.Errorf("err = %v, want ErrSandboxNotFound", err)
	}
}

func TestCreateFailureDoesNotLeakEntry(t *testing.T) {
	// A runtime whose Create always fails must leave no residue in the map.
	m := NewManager(&failingRuntime{})
	t.Cleanup(m.Close)
	if _, err := m.Create(context.Background(), spec("boom")); err == nil {
		t.Fatal("expected create error")
	}
	if m.Get("boom") != nil {
		t.Error("failed sandbox left in manager map")
	}
	if len(m.Statuses()) != 0 {
		t.Errorf("statuses = %v, want empty", m.Statuses())
	}
}

func TestIdleSweepSkipsInFlight(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	sp := spec("busy", func(s *nodev1.SandboxSpec) {
		s.Lifecycle = &nodev1.Lifecycle{HasIdleTimeout: true, IdleTimeoutSeconds: 1, OnIdle: "kill"}
	})
	if _, err := m.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	// Hold an in-flight marker across the idle deadline.
	_, release, err := m.AgentConn(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if m.StateOf("busy") != runtime.StateRunning {
		t.Fatalf("in-flight sandbox was swept: state=%s", m.StateOf("busy"))
	}
	release()
	// After release it becomes eligible again.
	deadline := time.Now().Add(5 * time.Second)
	for m.Get("busy") != nil {
		if time.Now().After(deadline) {
			t.Fatal("sandbox not swept after release")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConcurrentPauseOnlyOneWins(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("race1")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var okCount atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Pause(ctx, "race1"); err == nil {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := okCount.Load(); got != 1 {
		t.Errorf("successful pauses = %d, want 1", got)
	}
	if m.StateOf("race1") != runtime.StatePaused {
		t.Errorf("state = %s", m.StateOf("race1"))
	}
}

// failingRuntime always fails Create.
type failingRuntime struct{}

func (f *failingRuntime) Name() string { return "failing" }
func (f *failingRuntime) Create(context.Context, *runtime.Spec) (*runtime.Handle, error) {
	return nil, errors.New("synthetic create failure")
}
func (f *failingRuntime) Destroy(context.Context, string, bool) error { return nil }
func (f *failingRuntime) Pause(context.Context, string) error         { return nil }
func (f *failingRuntime) Resume(context.Context, string) error        { return nil }
func (f *failingRuntime) List(context.Context) ([]string, error)      { return nil, nil }
