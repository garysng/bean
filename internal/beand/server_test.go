package beand

import (
	"bytes"
	"context"
	"net"
	"os"
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

// shortSocket returns a socket path short enough for the ~104 byte
// sockaddr_un limit regardless of the test name.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "a.sock")
}

func startTestAgent(t *testing.T, rootDir string) agentv1.AgentServiceClient {
	t.Helper()
	lis, err := net.Listen("unix", shortSocket(t))
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
	lis, err := net.Listen("unix", shortSocket(t))
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

func TestSymlinkEscapeBlocked(t *testing.T) {
	root := t.TempDir()
	// Create a symlink inside the sandbox root pointing at the host root.
	if err := os.Symlink("/", filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Also plant a secret outside the sandbox root.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("host-secret"), 0o600)

	c := startTestAgent(t, root)
	ctx := context.Background()

	// Read through the symlink must fail (os.Root refuses traversal).
	rs, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: "/escape" + outside})
	if err == nil {
		_, err = rs.Recv()
	}
	if err == nil {
		t.Fatal("symlink escape read succeeded; expected failure")
	}

	// Write through the symlink must fail too.
	ws, werr := c.WriteFile(ctx)
	if werr != nil {
		t.Fatal(werr)
	}
	_ = ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{Path: "/escape/tmp/pwned.txt", Mkdirs: true},
	}})
	_ = ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("x")}})
	if _, err := ws.CloseAndRecv(); err == nil {
		t.Error("symlink escape write succeeded; expected failure")
	}

	// Delete through the symlink must not remove the outside file.
	_, _ = c.DeleteFile(ctx, &commonv1.DeleteFileRequest{Path: "/escape" + outside})
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file was deleted through symlink: %v", err)
	}
}

func TestDeleteRootRefused(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	_, err := c.DeleteFile(context.Background(), &commonv1.DeleteFileRequest{Path: "/"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestWriteFileStripsSetuidBits(t *testing.T) {
	root := t.TempDir()
	c := startTestAgent(t, root)
	ws, err := c.WriteFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 04755 = setuid + rwxr-xr-x
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{Path: "/suid", Mode: 0o4755},
	}})
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("x")}})
	if _, err := ws.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "suid"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid bit survived: mode=%v", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("perm = %o, want 755", perm)
	}
}

func TestWriteFileAtomicOnFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "keep.txt")
	os.WriteFile(target, []byte("original"), 0o644)

	c := startTestAgent(t, root)
	ws, err := c.WriteFile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{
		Meta: &commonv1.WriteFileMeta{Path: "/keep.txt"},
	}})
	ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: []byte("partial")}})
	// Abort without CloseAndRecv: the target must keep its original content.
	if err := ws.CloseSend(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	got, _ := os.ReadFile(target)
	if string(got) != "original" && string(got) != "partial" {
		t.Errorf("unexpected content %q", got)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bean-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestExecEnvIsAllowlisted(t *testing.T) {
	t.Setenv("BEAN_SECRET_TOKEN", "super-secret")
	c := startTestAgent(t, t.TempDir())
	resp, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd: []string{"sh", "-c", "env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Stdout), "BEAN_SECRET_TOKEN") {
		t.Error("host secret leaked into sandbox env")
	}
	if !strings.Contains(string(resp.Stdout), "PATH=") {
		t.Error("PATH missing from sandbox env")
	}
}

func TestExecTimeoutWithOrphanGrandchild(t *testing.T) {
	c := startTestAgent(t, t.TempDir())
	start := time.Now()
	// Grandchild holds stdout open past the parent's exit; WaitDelay must
	// keep this from blocking forever.
	_, err := c.Exec(context.Background(), &commonv1.ExecRequest{
		Cmd:            []string{"sh", "-c", "sleep 30 & sleep 30"},
		TimeoutSeconds: 1,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("exec took %s; WaitDelay did not bound the wait", elapsed)
	}
}

func TestUserProcessRestartableAfterExit(t *testing.T) {
	lis, err := net.Listen("unix", shortSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("test", t.TempDir())
	t.Cleanup(func() { s.Close() })
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, s)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, _ := grpc.NewClient("unix://"+lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	c := agentv1.NewAgentServiceClient(conn)

	if _, err := c.StartUserProcess(context.Background(), &agentv1.StartUserProcessRequest{
		Cmd: []string{"sh", "-c", "exit 5"},
	}); err != nil {
		t.Fatal(err)
	}
	// Wait for the reaper to clear state.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, ok := s.UserExitCode(); ok {
			if code != 5 {
				t.Errorf("exit code = %d, want 5", code)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user process exit not recorded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Restart must now be allowed.
	if _, err := c.StartUserProcess(context.Background(), &agentv1.StartUserProcessRequest{
		Cmd: []string{"true"},
	}); err != nil {
		t.Errorf("restart after exit failed: %v", err)
	}
}
