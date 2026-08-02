package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
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
func (f *failingRuntime) Checkpoint(context.Context, string, io.Writer, runtime.CheckpointOptions) error {
	return errors.New("synthetic checkpoint failure")
}
func (f *failingRuntime) Restore(context.Context, *runtime.Spec, io.Reader) (*runtime.Handle, error) {
	return nil, errors.New("synthetic restore failure")
}
func (f *failingRuntime) Pause(context.Context, string) error    { return nil }
func (f *failingRuntime) Resume(context.Context, string) error   { return nil }
func (f *failingRuntime) List(context.Context) ([]string, error) { return nil, nil }

func TestManagerMetricsRecordPhases(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("met1")); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := m.Metrics().WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`bean_node_creates_total{outcome="success",runtime="local"} 1`,
		`phase="runtime_create"`,
		`phase="agent_ready"`,
		`phase="total"`,
		"bean_node_create_phase_seconds_count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestManagerMetricsRecordFailedCreate(t *testing.T) {
	m := NewManager(&failingRuntime{})
	t.Cleanup(m.Close)
	if _, err := m.Create(context.Background(), spec("boom")); err == nil {
		t.Fatal("expected failure")
	}
	var b strings.Builder
	m.Metrics().WritePrometheus(&b)
	if !strings.Contains(b.String(), `bean_node_creates_total{outcome="error",runtime="failing"} 1`) {
		t.Errorf("failed create not counted:\n%s", b.String())
	}
}

func TestManagerRefreshGauges(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("g1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	m.RefreshGauges()

	var b strings.Builder
	m.Metrics().WritePrometheus(&b)
	out := b.String()
	if !strings.Contains(out, `bean_node_sandboxes{state="PAUSED"} 1`) {
		t.Errorf("paused gauge wrong:\n%s", out)
	}
	// States with nothing in them report zero rather than being absent.
	if !strings.Contains(out, `bean_node_sandboxes{state="RUNNING"} 0`) {
		t.Errorf("running gauge not zeroed:\n%s", out)
	}
	if !strings.Contains(out, "bean_node_requests_in_flight 0") {
		t.Errorf("in-flight gauge missing:\n%s", out)
	}
}

func TestManagerMetricsCountDestroyAndIdleActions(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	sp := spec("idle-metric", func(s *nodev1.SandboxSpec) {
		s.Lifecycle = &nodev1.Lifecycle{HasIdleTimeout: true, IdleTimeoutSeconds: 1, OnIdle: "kill"}
	})
	if _, err := m.Create(ctx, sp); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Get("idle-metric") != nil {
		if time.Now().After(deadline) {
			t.Fatal("idle sweep did not fire")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var b strings.Builder
	m.Metrics().WritePrometheus(&b)
	out := b.String()
	if !strings.Contains(out, `bean_node_idle_actions_total{action="kill",outcome="success"} 1`) {
		t.Errorf("idle action not counted:\n%s", out)
	}
	if !strings.Contains(out, `bean_node_destroys_total{outcome="success",runtime="local"} 1`) {
		t.Errorf("destroy not counted:\n%s", out)
	}
}

func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("src")); err != nil {
		t.Fatal(err)
	}

	// Write a file through the agent so the checkpoint has something the
	// restored sandbox must be able to read back.
	conn, release, err := m.AgentConn(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	ac := agentv1.NewAgentServiceClient(conn)
	ws, err := ac.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{Path: "/state/data.txt", Mkdirs: true},
	}})
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("persisted-state")}})
	if _, err := ws.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	release()

	var buf bytes.Buffer
	if err := m.Snapshot(ctx, "src", &buf, runtime.CheckpointOptions{IncludeMemory: true}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty checkpoint")
	}
	// The source sandbox keeps running: a snapshot must not disturb it.
	if got := m.StateOf("src"); got != runtime.StateRunning {
		t.Errorf("source state = %s, want RUNNING", got)
	}
	if _, rel, err := m.AgentConn(ctx, "src"); err != nil {
		t.Errorf("source unusable after snapshot: %v", err)
	} else {
		rel()
	}

	// Restore into a new sandbox and verify the file came along.
	restored, err := m.RestoreSandbox(ctx, spec("dst"), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != runtime.StateRunning {
		t.Errorf("restored state = %s", restored.State)
	}
	conn2, release2, err := m.AgentConn(ctx, "dst")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	rs, err := agentv1.NewAgentServiceClient(conn2).ReadFile(ctx,
		&commonv1.ReadFileRequest{Path: "/state/data.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for {
		chunk, rerr := rs.Recv()
		if rerr != nil {
			break
		}
		data = append(data, chunk.Data...)
	}
	if string(data) != "persisted-state" {
		t.Errorf("restored content = %q, want persisted-state", data)
	}
}

func TestSnapshotOfPausedSandboxStaysPaused(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("p")); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := m.Snapshot(ctx, "p", &buf, runtime.CheckpointOptions{IncludeMemory: true}); err != nil {
		t.Fatal(err)
	}
	if got := m.StateOf("p"); got != runtime.StatePaused {
		t.Errorf("state = %s, want PAUSED preserved", got)
	}
}

func TestSnapshotRejectsBadStates(t *testing.T) {
	m := newTestManager(t)
	var buf bytes.Buffer
	opts := runtime.CheckpointOptions{IncludeMemory: true}
	if err := m.Snapshot(context.Background(), "ghost", &buf, opts); !errors.Is(err, ErrSandboxNotFound) {
		t.Errorf("err = %v, want ErrSandboxNotFound", err)
	}
}

func TestSnapshotFailureLeavesSandboxRunning(t *testing.T) {
	// A runtime whose Checkpoint fails must not leave the sandbox stuck in
	// SNAPSHOTTING.
	m := NewManager(&failingCheckpointRuntime{LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir())})
	t.Cleanup(m.Close)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("s")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := m.Snapshot(ctx, "s", &buf, runtime.CheckpointOptions{IncludeMemory: true}); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	if got := m.StateOf("s"); got != runtime.StateRunning {
		t.Errorf("state = %s, want RUNNING restored after failure", got)
	}
}

func TestRestoreDuplicateRejected(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, spec("dup")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreSandbox(ctx, spec("dup"), bytes.NewReader(nil)); err == nil {
		t.Error("expected duplicate rejection")
	}
}

func TestRestoreCorruptCheckpointFails(t *testing.T) {
	m := newTestManager(t)
	_, err := m.RestoreSandbox(context.Background(), spec("bad"),
		bytes.NewReader([]byte("not a checkpoint")))
	if err == nil {
		t.Fatal("expected restore failure")
	}
	// The failed sandbox must not linger in the manager.
	if m.Get("bad") != nil {
		t.Error("failed restore left an entry behind")
	}
}

// failingCheckpointRuntime behaves like LocalRuntime except Checkpoint.
type failingCheckpointRuntime struct {
	*runtime.LocalRuntime
}

func (f *failingCheckpointRuntime) Checkpoint(context.Context, string, io.Writer, runtime.CheckpointOptions) error {
	return errors.New("synthetic checkpoint failure")
}

// TestDestroyDoesNotWaitOnGuestShutdown pins the fix for a destroy that took
// 5.2s while the create it tore down took 1.0s.
//
// The runtime used to ask the guest to power off over ACPI and wait up to five
// seconds for it to exit. That wait could not succeed — the guest kernel has no
// CONFIG_ACPI_BUTTON and the agent is PID 1 without a signal handler — so every
// destroy paid the full timeout. A latency assertion is the only kind that
// catches a reintroduced blind wait: the old code was correct in every respect
// except that it was waiting for something that would never happen.
func TestDestroyDoesNotWaitOnGuestShutdown(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("sbx-destroy-fast")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := m.Destroy(ctx, "sbx-destroy-fast", false); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// The removed wait was 5s, and the flush that replaced it is bounded at 2s.
	// 3s therefore fails if the blind wait returns and passes on a slow machine.
	if elapsed > 3*time.Second {
		t.Errorf("graceful destroy took %s; a blind wait on guest shutdown is back", elapsed)
	}
	if m.Get("sbx-destroy-fast") != nil {
		t.Error("destroy left an entry behind")
	}
}

// TestDestroyPausedSandboxIsFast covers two ways destroying a paused sandbox
// used to stall, both of them a wait for something that could not happen.
//
// The pre-destroy flush must skip a paused sandbox: flushing goes through the
// agent, and the convenience path for reaching one transparently resumes it, so
// tearing down a paused sandbox would first boot it back up. A paused guest has
// also written nothing since it was paused.
//
// Separately, a paused process is stopped with SIGSTOP and does not act on
// SIGTERM — it stays queued until the process runs again — so the runtime's
// grace period always expired before the kill. That made destroying a paused
// sandbox slower than destroying a running one.
func TestDestroyPausedSandboxIsFast(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("sbx-destroy-paused")); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx, "sbx-destroy-paused"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := m.Destroy(ctx, "sbx-destroy-paused", false); err != nil {
		t.Fatal(err)
	}
	// Both removed waits were multi-second. A paused sandbox should tear down
	// about as fast as a running one, so this is well under either.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("destroying a paused sandbox took %s: either the flush resumed it "+
			"or the SIGTERM was delivered to a stopped process", elapsed)
	}
}
