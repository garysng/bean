package node

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// buildingRuntime is LocalRuntime plus a scripted BuildImage. BuildKit is not
// available in unit tests, and the behaviour under test is the streaming and
// cancellation around the builder rather than the builder itself: what matters
// is that log writes reach the client while the build is still running, and that
// cancelling the call reaches the build.
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

// startBuildNode brings up SandboxService over a runtime that can build.
func startBuildNode(t *testing.T, rt runtime.Runtime) nodev1.SandboxServiceClient {
	t.Helper()
	mgr := NewManager(rt)
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, NewGRPCServer(mgr))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodev1.NewSandboxServiceClient(conn)
}

func buildReq(tag string) *nodev1.BuildImageRequest {
	return &nodev1.BuildImageRequest{Tag: tag, Dockerfile: "FROM scratch"}
}

// TestBuildImageStreamsLogsBeforeFinishing is the assertion that makes streaming
// worth having: the first frames must arrive while the build is still running.
// A build that buffered its output and sent it with the result would pass every
// "the logs came through" check and still tell nobody which layer is slow.
func TestBuildImageStreamsLogsBeforeFinishing(t *testing.T) {
	release := make(chan struct{})
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			io.WriteString(req.Logs, "#1 [1/2] FROM scratch\n")
			// Blocks until the test has read the line above, so a passing run
			// proves the frame was not held back until the build returned.
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			io.WriteString(req.Logs, "#2 exporting to client\n")
			return req.Tag, nil
		},
	}
	c := startBuildNode(t, rt)

	// Bounded because the failure this test exists to catch is a hang, not a
	// wrong value: an implementation that accumulates frames and sends them at
	// the end never delivers the first one, so an unbounded Recv waits for the
	// build, which is waiting for the Recv. Without a deadline that deadlock
	// takes down the whole package's test binary and every other result with it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := c.BuildImage(ctx, buildReq("stream:v1"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("first frame: %v (a build that buffers its output until it "+
			"finishes cannot deliver one, since the build is blocked on this read)", err)
	}
	if got := string(ev.GetLog()); !strings.Contains(got, "FROM scratch") {
		t.Fatalf("first frame log = %q, want the first build line", got)
	}
	close(release)

	var logs strings.Builder
	var ref string
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		logs.Write(ev.GetLog())
		if res := ev.GetResult(); res != nil {
			ref = res.ImageRef
		}
	}
	if ref != "stream:v1" {
		t.Errorf("result ref = %q, want stream:v1", ref)
	}
	if !strings.Contains(logs.String(), "exporting to client") {
		t.Errorf("later output missing: %q", logs.String())
	}
}

// TestBuildImageCancelStopsTheBuild covers the mechanism the whole feature rests
// on: aborting the call has to reach the builder's context, since that is what
// kills buildctl on a real node.
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
	c := startBuildNode(t, rt)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.BuildImage(ctx, buildReq("cancel:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	<-started
	cancel()

	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("builder saw %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the call did not reach the builder")
	}
}

// TestBuildImageFailureIsNotAResultFrame keeps a failed build from looking like
// a finished one. The result arrives as a frame and the failure as the stream's
// status, so a caller that checks only Recv's error cannot confuse them.
func TestBuildImageFailureIsNotAResultFrame(t *testing.T) {
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			io.WriteString(req.Logs, "#4 ERROR: process did not complete\n")
			return "", errors.New("image: build failed: exit status 1")
		},
	}
	c := startBuildNode(t, rt)

	stream, err := c.BuildImage(context.Background(), buildReq("fail:v1"))
	if err != nil {
		t.Fatal(err)
	}
	var sawResult bool
	var logs strings.Builder
	var recvErr error
	for {
		ev, err := stream.Recv()
		if err != nil {
			recvErr = err
			break
		}
		logs.Write(ev.GetLog())
		if ev.GetResult() != nil {
			sawResult = true
		}
	}
	if recvErr == io.EOF {
		t.Fatal("failed build ended the stream cleanly; a caller would call it a success")
	}
	if status.Code(recvErr) != codes.Internal {
		t.Errorf("code = %s, want Internal: %v", status.Code(recvErr), recvErr)
	}
	if sawResult {
		t.Error("failed build sent a result frame")
	}
	// The output has to survive the failure: it names the failing step, which is
	// the only thing that tells anyone what to fix.
	if !strings.Contains(logs.String(), "did not complete") {
		t.Errorf("failure lost its logs: %q", logs.String())
	}
}

// TestBuildImageCancelReportsCanceled pins the code, because the control plane
// tells "someone stopped this" apart from "this node cannot build" by it, and
// only the second is worth alerting on.
func TestBuildImageCancelReportsCanceled(t *testing.T) {
	rt := &buildingRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		build: func(ctx context.Context, req runtime.BuildRequest) (string, error) {
			<-ctx.Done()
			// A killed subprocess reports an exit status, not a cancellation,
			// which is what the server has to see through.
			return "", errors.New("signal: killed")
		},
	}
	mgr := NewManager(rt)
	t.Cleanup(mgr.Close)
	srv := NewGRPCServer(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	stream := &recordingBuildStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- srv.BuildImage(buildReq("cancel:v2"), stream) }()
	cancel()

	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Errorf("code = %s, want Canceled: %v", status.Code(err), err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
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

// recordingBuildStream is a minimal server stream, for driving the handler
// without a socket.
type recordingBuildStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *recordingBuildStream) Context() context.Context           { return s.ctx }
func (s *recordingBuildStream) Send(*nodev1.BuildImageEvent) error { return nil }
