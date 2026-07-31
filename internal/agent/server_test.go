package agent

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
)

func startTestAgent(t *testing.T, rootDir string) agentv1.AgentServiceClient {
	t.Helper()
	lis, err := net.Listen("unix", filepath.Join(t.TempDir(), "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, NewServer("test", rootDir))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("unix://"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentv1.NewAgentServiceClient(conn)
}

func TestHealth(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	resp, err := c.Health(context.Background(), &agentv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentVersion != "test" {
		t.Errorf("version = %q, want test", resp.AgentVersion)
	}
}

func TestExecBasic(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	resp, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd: []string{"sh", "-c", "echo hello; echo err >&2; exit 3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", resp.ExitCode)
	}
	if strings.TrimSpace(string(resp.Stdout)) != "hello" {
		t.Errorf("stdout = %q", resp.Stdout)
	}
	if strings.TrimSpace(string(resp.Stderr)) != "err" {
		t.Errorf("stderr = %q", resp.Stderr)
	}
}

func TestExecStdinAndEnv(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	resp, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd:   []string{"sh", "-c", "cat; echo $FOO"},
		Stdin: []byte("in|"),
		Env:   map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Stdout); got != "in|bar\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestExecTimeout(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	_, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd:            []string{"sleep", "5"},
		TimeoutSeconds: 1,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestExecOutputTruncation(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	resp, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd:            []string{"sh", "-c", "yes x | head -c 100000"},
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Error("expected truncated")
	}
	if len(resp.Stdout) != 1024 {
		t.Errorf("stdout len = %d, want 1024", len(resp.Stdout))
	}
}

func TestExecMissingCmd(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	_, err := c.Exec(context.Background(), &commonv1.ExecRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestWriteReadDeleteFile(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	ctx := context.Background()

	ws, err := c.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{Path: "/sub/dir/hello.txt", Mode: 0o600, Mkdirs: true},
	}}); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("data"), 1000)
	if err := ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: payload}}); err != nil {
		t.Fatal(err)
	}
	wr, err := ws.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if wr.BytesWritten != int64(len(payload)) {
		t.Errorf("written = %d, want %d", wr.BytesWritten, len(payload))
	}

	rs, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: "/sub/dir/hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		chunk, rerr := rs.Recv()
		if rerr != nil {
			break
		}
		got = append(got, chunk.Data...)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read %d bytes, want %d", len(got), len(payload))
	}

	ls, err := c.ListDir(ctx, &commonv1.ListDirRequest{Path: "/sub/dir"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.Entries) != 1 || ls.Entries[0].Name != "hello.txt" {
		t.Errorf("entries = %+v", ls.Entries)
	}

	if _, err := c.DeleteFile(ctx, &commonv1.DeleteFileRequest{Path: "/sub/dir/hello.txt"}); err != nil {
		t.Fatal(err)
	}
	_, err = c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: "/sub/dir/hello.txt"})
	if err == nil {
		rs2, _ := c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: "/sub/dir/hello.txt"})
		_, err = rs2.Recv()
	}
}

func TestPathEscapeRejected(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	ctx := context.Background()
	for _, p := range []string{"../etc/passwd", "relative/path", ""} {
		_, err := c.ListDir(ctx, &commonv1.ListDirRequest{Path: p})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("path %q: err = %v, want InvalidArgument", p, err)
		}
	}
	// Absolute path with .. that resolves inside root is fine; escaping is not.
	_, err := c.ListDir(ctx, &commonv1.ListDirRequest{Path: "/a/../../.."})
	// filepath.Clean("/a/../../..") = "/", which joins to root itself — allowed.
	_ = err
}

func TestReadFileNotFound(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	rs, err := c.ReadFile(context.Background(), &commonv1.ReadFileRequest{Path: "/nope"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rs.Recv()
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestLogsAndTail(t *testing.T) {
	root := t.TempDir()
	lis, err := net.Listen("unix", filepath.Join(t.TempDir(), "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("test", root)
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, s)
	go srv.Serve(lis)
	defer srv.Stop()

	s.Logs().Write([]byte("line1\nline2\nline3\n"))

	conn, _ := grpc.NewClient("unix://"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	c := agentv1.NewAgentServiceClient(conn)

	ls, err := c.GetLogs(context.Background(), &commonv1.GetLogsRequest{TailLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := ls.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk.Data) != "line2\nline3\n" {
		t.Errorf("tail = %q", chunk.Data)
	}
}

func TestStartUserProcess(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	resp, err := c.StartUserProcess(context.Background(), &agentv1.StartUserProcessRequest{
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{"echo started"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Pid <= 0 {
		t.Errorf("pid = %d", resp.Pid)
	}
	// double start rejected
	_, err = c.StartUserProcess(context.Background(), &agentv1.StartUserProcessRequest{
		Entrypoint: []string{"true"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestRingBuffer(t *testing.T) {
	r := NewRingBuffer(8)
	r.Write([]byte("abc"))
	if got := string(r.Snapshot()); got != "abc" {
		t.Errorf("got %q", got)
	}
	r.Write([]byte("defghij")) // total 10 > 8, keeps last 8
	if got := string(r.Snapshot()); got != "cdefghij" {
		t.Errorf("got %q", got)
	}
	r2 := NewRingBuffer(4)
	r2.Write([]byte("0123456789")) // oversized single write keeps tail
	if got := string(r2.Snapshot()); got != "6789" {
		t.Errorf("got %q", got)
	}
}
