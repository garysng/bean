package node

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// startNodeGRPC brings up SandboxService over a real socket.
func startNodeGRPC(t *testing.T) (nodev1.SandboxServiceClient, *Manager) {
	t.Helper()
	mgr := NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, NewGRPCServer(mgr, nil))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodev1.NewSandboxServiceClient(conn), mgr
}

func TestGRPCCreateGetDestroy(t *testing.T) {
	c, _ := startNodeGRPC(t)
	ctx := context.Background()

	if _, err := c.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty spec: err = %v, want InvalidArgument", err)
	}
	resp, err := c.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{Spec: spec("g1")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status.State != "RUNNING" {
		t.Errorf("state = %s", resp.Status.State)
	}
	got, err := c.GetSandbox(ctx, &nodev1.GetSandboxRequest{SandboxId: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.StartedAtUnix == 0 {
		t.Error("startedAt not populated")
	}
	if _, err := c.DestroySandbox(ctx, &nodev1.DestroySandboxRequest{SandboxId: "g1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSandbox(ctx, &nodev1.GetSandboxRequest{SandboxId: "g1"}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: err = %v, want NotFound", err)
	}
}

func TestGRPCUnknownSandboxIsNotFound(t *testing.T) {
	c, _ := startNodeGRPC(t)
	ctx := context.Background()
	// Data-plane calls against an unknown sandbox must map to NotFound (404
	// at the gateway), not FailedPrecondition.
	if _, err := c.Exec(ctx, &commonv1.ExecRequest{SandboxId: "ghost", Cmd: []string{"true"}}); status.Code(err) != codes.NotFound {
		t.Errorf("exec: err = %v, want NotFound", err)
	}
	if _, err := c.ListDir(ctx, &commonv1.ListDirRequest{SandboxId: "ghost", Path: "/"}); status.Code(err) != codes.NotFound {
		t.Errorf("listdir: err = %v, want NotFound", err)
	}
	if _, err := c.DeleteFile(ctx, &commonv1.DeleteFileRequest{SandboxId: "ghost", Path: "/x"}); status.Code(err) != codes.NotFound {
		t.Errorf("deletefile: err = %v, want NotFound", err)
	}
	st, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{SandboxId: "ghost", Path: "/x"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("readfile: err = %v, want NotFound", err)
	}
	ls, lerr := c.GetLogs(ctx, &commonv1.GetLogsRequest{SandboxId: "ghost"})
	if lerr == nil {
		_, lerr = ls.Recv()
	}
	if status.Code(lerr) != codes.NotFound {
		t.Errorf("getlogs: err = %v, want NotFound", lerr)
	}
}

func TestGRPCPauseResumeAndStartUserProcess(t *testing.T) {
	c, mgr := startNodeGRPC(t)
	ctx := context.Background()
	sp := spec("g2")
	sp.Cmd = []string{"sh", "-c", "sleep 30"}
	if _, err := c.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{Spec: sp}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PauseSandbox(ctx, &nodev1.PauseSandboxRequest{SandboxId: "g2"}); err != nil {
		t.Fatal(err)
	}
	if mgr.StateOf("g2") != runtime.StatePaused {
		t.Fatalf("state = %s", mgr.StateOf("g2"))
	}
	if _, err := c.ResumeSandbox(ctx, &nodev1.ResumeSandboxRequest{SandboxId: "g2"}); err != nil {
		t.Fatal(err)
	}
	if mgr.StateOf("g2") != runtime.StateRunning {
		t.Fatalf("state = %s", mgr.StateOf("g2"))
	}

	resp, err := c.StartUserProcess(ctx, &nodev1.StartUserProcessNodeRequest{SandboxId: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Pid <= 0 {
		t.Errorf("pid = %d", resp.Pid)
	}
	if _, err := c.StartUserProcess(ctx, &nodev1.StartUserProcessNodeRequest{SandboxId: "ghost"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown sandbox: err = %v, want NotFound", err)
	}
	// Pause/Resume of unknown sandbox
	if _, err := c.PauseSandbox(ctx, &nodev1.PauseSandboxRequest{SandboxId: "ghost"}); err == nil {
		t.Error("pause ghost: expected error")
	}
	if _, err := c.ResumeSandbox(ctx, &nodev1.ResumeSandboxRequest{SandboxId: "ghost"}); err == nil {
		t.Error("resume ghost: expected error")
	}
}

func TestGRPCFileRoundTripAndLogs(t *testing.T) {
	c, _ := startNodeGRPC(t)
	ctx := context.Background()
	if _, err := c.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{Spec: spec("g3")}); err != nil {
		t.Fatal(err)
	}

	ws, err := c.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{SandboxId: "g3", Path: "/d/f.txt", Mkdirs: true},
	}})
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("payload")}})
	wr, err := ws.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if wr.BytesWritten != 7 {
		t.Errorf("written = %d", wr.BytesWritten)
	}

	rs, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{SandboxId: "g3", Path: "/d/f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for {
		chunk, rerr := rs.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
		data = append(data, chunk.Data...)
	}
	if string(data) != "payload" {
		t.Errorf("read = %q", data)
	}

	ld, err := c.ListDir(ctx, &commonv1.ListDirRequest{SandboxId: "g3", Path: "/d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ld.Entries) != 1 || ld.Entries[0].Name != "f.txt" {
		t.Errorf("entries = %+v", ld.Entries)
	}

	// Logs stream terminates cleanly even when empty.
	ls, err := c.GetLogs(ctx, &commonv1.GetLogsRequest{SandboxId: "g3"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, rerr := ls.Recv(); rerr != nil {
			if rerr != io.EOF {
				t.Fatalf("logs: %v", rerr)
			}
			break
		}
	}
}

func TestGRPCWriteFileRequiresMetaFirst(t *testing.T) {
	c, _ := startNodeGRPC(t)
	ws, err := c.WriteFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("x")}})
	if _, err := ws.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}
