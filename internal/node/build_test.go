package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/s3"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// buildingRuntime is LocalRuntime plus a scripted BuildImage. BuildKit is not
// available in unit tests, and the behaviour under test is the log upload and
// cancellation around the builder rather than the builder itself: what matters
// is that a build's output reaches the shared store while the build is still
// running, and that cancelling reaches the build.
type buildingRuntime struct {
	*runtime.LocalRuntime
	// build is the scripted body. It receives the request so it can write logs.
	build func(ctx context.Context, req runtime.BuildRequest) (string, error)
}

func (b *buildingRuntime) BuildImage(ctx context.Context, req runtime.BuildRequest) (runtime.BuildResult, error) {
	ref, err := b.build(ctx, req)
	if err != nil {
		return runtime.BuildResult{}, err
	}
	return runtime.BuildResult{ImageRef: ref}, nil
}

// startBuildNode brings up SandboxService over a runtime that can build, with a
// local-directory log store the test can read back. It returns the client and
// the store so an assertion can inspect what the node uploaded.
func startBuildNode(t *testing.T, rt runtime.Runtime) (nodev1.SandboxServiceClient, s3.ObjectStore) {
	t.Helper()
	logs, err := s3.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(rt)
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, NewGRPCServer(mgr, logs))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodev1.NewSandboxServiceClient(conn), logs
}

func buildReq(tag string) *nodev1.BuildImageRequest {
	return &nodev1.BuildImageRequest{Tag: tag, Dockerfile: "FROM scratch"}
}

// waitBuildStatus polls GetBuildStatus until the build reaches a terminal phase
// (SUCCEEDED or FAILED) or the deadline elapses, mirroring how the control plane
// learns a build's outcome. It fails the test on timeout.
func waitBuildStatus(t *testing.T, c nodev1.SandboxServiceClient, tag string) *nodev1.GetBuildStatusResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.GetBuildStatus(context.Background(), &nodev1.GetBuildStatusRequest{Tag: tag})
		if err != nil {
			t.Fatalf("GetBuildStatus: %v", err)
		}
		switch resp.GetPhase() {
		case nodev1.BuildPhase_BUILD_SUCCEEDED, nodev1.BuildPhase_BUILD_FAILED:
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("build %s did not reach a terminal status within deadline", tag)
	return nil
}

// readAllLogs drains a build's log from the store.
func readAllLogs(t *testing.T, store s3.ObjectStore, ref string) string {
	t.Helper()
	r, err := s3.NewBuildLogReader(store, ref)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := r.ReadFrom(context.Background(), 0, &buf); err != nil {
		t.Fatalf("read logs: %v", err)
	}
	return buf.String()
}

// TestBuildImageUploadsLogsWhileRunning is the assertion that makes the log
// upload worth having: output must reach the shared store while the build is
// still running, so a follower sees which layer is slow. A build that uploaded
// only at the end would pass a "the logs came through" check and still tell
// nobody about a running build.
func TestBuildImageUploadsLogsWhileRunning(t *testing.T) {
	release := make(chan struct{})
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			io.WriteString(req.Logs, "#1 [1/2] FROM scratch\n")
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			io.WriteString(req.Logs, "#2 exporting to client\n")
			return req.Tag, nil
		},
	}
	c, store := startBuildNode(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.StartBuild(ctx, buildReq("stream:v1")); err != nil {
		t.Fatal(err)
	}

	// The first line is written before the build blocks on release, and the writer
	// flushes on a short interval, so it should appear in the store while the build
	// is still running -- before we let it finish. StartBuild has already returned
	// (it does not wait for the build), so this genuinely observes a running build.
	reader, _ := s3.NewBuildLogReader(store, "stream:v1")
	deadline := time.Now().Add(10 * time.Second)
	var early string
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		reader.ReadFrom(ctx, 0, &buf)
		if strings.Contains(buf.String(), "FROM scratch") {
			early = buf.String()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(early, "FROM scratch") {
		t.Fatalf("first line did not reach the store while the build ran; got %q "+
			"(a build that uploaded only at the end could not deliver it, since the "+
			"build is blocked on release)", early)
	}
	// While blocked, the build must report RUNNING, not a terminal phase.
	if resp, err := c.GetBuildStatus(ctx, &nodev1.GetBuildStatusRequest{Tag: "stream:v1"}); err != nil {
		t.Fatalf("GetBuildStatus: %v", err)
	} else if resp.GetPhase() != nodev1.BuildPhase_BUILD_RUNNING {
		t.Errorf("phase while running = %v, want BUILD_RUNNING", resp.GetPhase())
	}
	close(release)

	// Poll to terminal; the result carries the image ref.
	resp := waitBuildStatus(t, c, "stream:v1")
	if resp.GetPhase() != nodev1.BuildPhase_BUILD_SUCCEEDED {
		t.Fatalf("phase = %v, want SUCCEEDED (reason %q)", resp.GetPhase(), resp.GetReason())
	}
	if ref := resp.GetResult().GetImageRef(); ref != "stream:v1" {
		t.Errorf("result ref = %q, want stream:v1", ref)
	}
	// The full log, including output after the block, is in the store with a
	// terminal manifest.
	full := readAllLogs(t, store, "stream:v1")
	if !strings.Contains(full, "exporting to client") {
		t.Errorf("later output missing from store: %q", full)
	}
	if m, err := reader.Manifest(ctx); err != nil || !m.Done || m.Failed {
		t.Errorf("manifest = %+v err=%v, want done+success", m, err)
	}
}

// TestBuildImageCancelStopsTheBuild covers the cancel path the feature rests on:
// CancelBuild must reach the builder's context, since that is what kills buildctl
// on a real node -- and it must work as a separate call, not by aborting the
// build stream.
func TestBuildImageCancelStopsTheBuild(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan error, 1)
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			io.WriteString(req.Logs, "#1 [1/2] RUN sleep forever\n")
			close(started)
			<-ctx.Done()
			stopped <- ctx.Err()
			return "", ctx.Err()
		},
	}
	c, _ := startBuildNode(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.StartBuild(ctx, buildReq("cancel:v1")); err != nil {
		t.Fatal(err)
	}
	// Wait for the build to be underway, then cancel through the separate RPC --
	// the path a control-plane replica uses.
	<-started
	resp, err := c.CancelBuild(ctx, &nodev1.CancelBuildRequest{Tag: "cancel:v1"})
	if err != nil {
		t.Fatalf("CancelBuild: %v", err)
	}
	if !resp.GetFound() {
		t.Error("CancelBuild reported no build found for a running build")
	}

	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("builder saw %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CancelBuild did not reach the builder")
	}
	// The build's recorded outcome is FAILED with a cancellation reason -- the
	// control plane tells cancel apart from a build failure by that reason.
	st := waitBuildStatus(t, c, "cancel:v1")
	if st.GetPhase() != nodev1.BuildPhase_BUILD_FAILED {
		t.Errorf("phase = %v, want FAILED", st.GetPhase())
	}
	if !strings.Contains(st.GetReason(), "cancel") {
		t.Errorf("reason = %q, want it to mention cancellation", st.GetReason())
	}
}

// TestCancelUnknownBuildIsHarmless pins the idempotence CancelBuild promises: a
// tag with no live build reports found=false rather than erroring, so a racing
// double-cancel or a cancel for a finished build is safe.
func TestCancelUnknownBuildIsHarmless(t *testing.T) {
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build:        func(ctx context.Context, req runtime.BuildRequest) (string, error) { return req.Tag, nil },
	}
	c, _ := startBuildNode(t, rt)
	resp, err := c.CancelBuild(context.Background(), &nodev1.CancelBuildRequest{Tag: "never:built"})
	if err != nil {
		t.Fatalf("CancelBuild on unknown tag errored: %v", err)
	}
	if resp.GetFound() {
		t.Error("CancelBuild found a build that was never started")
	}
}

// TestBuildImageFailureIsReportedAsFailedPhase keeps a failed build from looking
// like a finished one: GetBuildStatus reports BUILD_FAILED with the error as the
// reason, never BUILD_SUCCEEDED, so a poller cannot confuse the two. The failing
// output is preserved in the store, where it names the failing step.
func TestBuildImageFailureIsReportedAsFailedPhase(t *testing.T) {
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			io.WriteString(req.Logs, "#4 ERROR: process did not complete\n")
			return "", errors.New("image: build failed: exit status 1")
		},
	}
	c, store := startBuildNode(t, rt)

	if _, err := c.StartBuild(context.Background(), buildReq("fail:v1")); err != nil {
		t.Fatal(err)
	}
	resp := waitBuildStatus(t, c, "fail:v1")
	if resp.GetPhase() != nodev1.BuildPhase_BUILD_FAILED {
		t.Fatalf("phase = %v, want BUILD_FAILED", resp.GetPhase())
	}
	if resp.GetResult() != nil {
		t.Error("failed build reported a result")
	}
	if !strings.Contains(resp.GetReason(), "build failed") {
		t.Errorf("reason = %q, want the build error", resp.GetReason())
	}
	// The output has to survive the failure: it names the failing step, which is
	// the only thing that tells anyone what to fix. It is in the store, with a
	// terminal failed manifest.
	logs := readAllLogs(t, store, "fail:v1")
	if !strings.Contains(logs, "did not complete") {
		t.Errorf("failure lost its logs: %q", logs)
	}
	r, _ := s3.NewBuildLogReader(store, "fail:v1")
	if m, err := r.Manifest(context.Background()); err != nil || !m.Done || !m.Failed {
		t.Errorf("manifest = %+v err=%v, want done+failed", m, err)
	}
}

// TestBuildImageCancelReportsCanceled pins the reason, because the control plane
// tells "someone stopped this" apart from "this node cannot build" by it, and
// only the second is worth alerting on. A cancelled build must record "build
// cancelled" even though the builder itself reports a subprocess exit status.
// It drives the server in-process (no socket) via StartBuild + GetBuildStatus.
func TestBuildImageCancelReportsCanceled(t *testing.T) {
	started := make(chan struct{})
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			close(started)
			<-ctx.Done()
			// A killed subprocess reports an exit status, not a cancellation,
			// which is what the server has to see through.
			return "", errors.New("signal: killed")
		},
	}
	logs, err := s3.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := NewGRPCServer(mgr, logs)

	ctx := context.Background()
	if _, err := srv.StartBuild(ctx, buildReq("cancel:v2")); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := srv.CancelBuild(ctx, &nodev1.CancelBuildRequest{Tag: "cancel:v2"}); err != nil {
		t.Fatalf("CancelBuild: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := srv.GetBuildStatus(ctx, &nodev1.GetBuildStatusRequest{Tag: "cancel:v2"})
		if err != nil {
			t.Fatalf("GetBuildStatus: %v", err)
		}
		if resp.GetPhase() == nodev1.BuildPhase_BUILD_FAILED {
			if !strings.Contains(resp.GetReason(), "cancel") {
				t.Errorf("reason = %q, want it to mention cancellation", resp.GetReason())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("build did not report a terminal (cancelled) status after CancelBuild")
}

// TestBuildOutcomeSeparatesCancelFromFailure guards the metric label. A build
// somebody stopped says nothing about this node's health, and counting the two
// together is how a broken builder hides behind ordinary cancellations.
func TestBuildOutcomeSeparatesCancelFromFailure(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if got := buildOutcome(context.Background(), nil); got != "success" {
		t.Errorf("success = %q", got)
	}
	if got := buildOutcome(context.Background(), errors.New("boom")); got != "error" {
		t.Errorf("failure = %q", got)
	}
	// The error is a subprocess exit status, as it is in reality: only the
	// context says the build was stopped on purpose.
	if got := buildOutcome(cancelled, errors.New("signal: killed")); got != "cancelled" {
		t.Errorf("cancelled = %q, want cancelled", got)
	}
}
