// Package beand implements the in-sandbox daemon (beand) gRPC service.
package beand

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	"github.com/garysng/bean/internal/logging"
)

const (
	defaultMaxOutputBytes = 1 << 20 // 1 MiB
	fileChunkSize         = 1 << 20
)

// Server implements agentv1.AgentServiceServer.
type Server struct {
	agentv1.UnimplementedAgentServiceServer

	version   string
	startedAt time.Time
	// rootDir confines file operations; "" means host root (production PID1).
	rootDir string
	// root is an os.Root handle on rootDir. All file operations go through it
	// so symlinks cannot escape the sandbox (os.Root refuses traversal out).
	root *os.Root
	logs *RingBuffer

	mu           sync.Mutex
	userProc     *os.Process
	userExitCode *int
}

func NewServer(version, rootDir string) *Server {
	s := &Server{
		version:   version,
		startedAt: time.Now(),
		rootDir:   rootDir,
		logs:      NewRingBuffer(8 << 20),
	}
	dir := rootDir
	if dir == "" {
		dir = "/"
	}
	if root, err := os.OpenRoot(dir); err == nil {
		s.root = root
	}
	return s
}

// Close releases the root handle.
func (s *Server) Close() error {
	if s.root != nil {
		return s.root.Close()
	}
	return nil
}

// Logs exposes the user-process log buffer (writer side used by process manager).
func (s *Server) Logs() *RingBuffer { return s.logs }

func (s *Server) Health(ctx context.Context, _ *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return &agentv1.HealthResponse{
		AgentVersion:  s.version,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}, nil
}

// rootRelative validates a sandbox-visible absolute path and returns it
// relative to the sandbox root, for use with os.Root operations. os.Root
// itself refuses symlink traversal outside the root, so this only handles
// argument validation and the absolute -> relative conversion.
func (s *Server) rootRelative(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", status.Error(codes.InvalidArgument, "path must be absolute")
	}
	if s.root == nil {
		return "", status.Error(codes.Internal, "sandbox root unavailable")
	}
	rel := strings.TrimPrefix(filepath.Clean(p), "/")
	if rel == "" {
		rel = "."
	}
	return rel, nil
}

// hostPath maps a sandbox-visible path to a host path for process cwd.
// Unlike file ops it cannot use os.Root (exec needs a real path), so it
// keeps the lexical containment check.
func (s *Server) hostPath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", status.Error(codes.InvalidArgument, "path must be absolute")
	}
	cleaned := filepath.Clean(p)
	if s.rootDir == "" {
		return cleaned, nil
	}
	joined := filepath.Join(s.rootDir, cleaned)
	root := filepath.Clean(s.rootDir)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", status.Error(codes.InvalidArgument, "path escapes sandbox root")
	}
	return joined, nil
}

// fsErr maps filesystem errors to gRPC status codes.
func fsErr(op string, err error) error {
	switch {
	case os.IsNotExist(err):
		return status.Errorf(codes.NotFound, "%s: not found", op)
	case os.IsPermission(err):
		return status.Errorf(codes.PermissionDenied, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func (s *Server) Exec(ctx context.Context, req *commonv1.ExecRequest) (*commonv1.ExecResponse, error) {
	if len(req.Cmd) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cmd is required")
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	maxOut := req.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutputBytes
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, req.Cmd[0], req.Cmd[1:]...)
	if req.Cwd != "" {
		cwd, err := s.hostPath(req.Cwd)
		if err != nil {
			return nil, err
		}
		cmd.Dir = cwd
	} else if s.rootDir != "" {
		cmd.Dir = s.rootDir
	}
	cmd.Env = buildEnv(req.Env)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	// Run in its own process group and kill the whole group on timeout,
	// then bound the wait so a grandchild holding the output pipe cannot
	// block Wait forever.
	cmd.SysProcAttr = execSysProcAttr()
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	stdout := newCappedBuffer(maxOut)
	stderr := newCappedBuffer(maxOut)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	// The agent cannot export spans, so this line is how work inside the guest
	// becomes visible: it carries the caller's trace id, which makes the gap
	// between the node's Exec span and the command's own runtime attributable
	// instead of a guess.
	logging.From(ctx).Info("exec finished",
		"cmd", req.Cmd[0], "durationMs", dur.Milliseconds())

	resp := &commonv1.ExecResponse{
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		Truncated:  stdout.Truncated() || stderr.Truncated(),
		DurationMs: dur.Milliseconds(),
	}
	var exitErr *exec.ExitError
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return nil, status.Errorf(codes.DeadlineExceeded, "exec timed out after %s", timeout)
	case err == nil:
		resp.ExitCode = 0
	case errors.As(err, &exitErr):
		resp.ExitCode = int32(exitErr.ExitCode())
	default:
		return nil, status.Errorf(codes.Internal, "exec failed: %v", err)
	}
	return resp, nil
}

func (s *Server) ReadFile(req *commonv1.ReadFileRequest, stream agentv1.AgentService_ReadFileServer) error {
	return s.readFileTo(req, func(chunk *commonv1.FileChunk) error { return stream.Send(chunk) })
}

// readFileTo is the transport-independent core of ReadFile: it opens the file and
// hands each chunk to send. The grpc-go method and the Connect adapter both wrap
// it, so one implementation serves both and the file-safety logic lives once.
func (s *Server) readFileTo(req *commonv1.ReadFileRequest, send func(*commonv1.FileChunk) error) error {
	rel, err := s.rootRelative(req.Path)
	if err != nil {
		return err
	}
	f, err := s.root.Open(rel)
	if err != nil {
		return fsErr("open "+req.Path, err)
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if serr := send(&commonv1.FileChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return status.Errorf(codes.Internal, "read: %v", rerr)
		}
	}
}

func (s *Server) WriteFile(stream agentv1.AgentService_WriteFileServer) error {
	resp, err := s.writeFileFrom(stream.Recv)
	if err != nil {
		return err
	}
	return stream.SendAndClose(resp)
}

// writeFileFrom is the transport-independent core of WriteFile: it pulls frames
// via recv (a meta frame then data frames) and returns the response. The grpc-go
// method sends it with SendAndClose; the Connect adapter returns it directly. A
// recv returning io.EOF ends the stream, exactly as grpc-go and Connect both
// report a client's half-close.
func (s *Server) writeFileFrom(recv func() (*commonv1.WriteFileFrame, error)) (*commonv1.WriteFileResponse, error) {
	first, err := recv()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "missing meta frame: %v", err)
	}
	meta := first.GetMeta()
	if meta == nil {
		return nil, status.Error(codes.InvalidArgument, "first frame must be meta")
	}
	rel, err := s.rootRelative(meta.Path)
	if err != nil {
		return nil, err
	}
	if meta.Mkdirs {
		if err := s.mkdirAllRoot(filepath.Dir(rel)); err != nil {
			return nil, fsErr("mkdirs "+meta.Path, err)
		}
	}
	// Mask off setuid/setgid/sticky: callers may not create privileged files.
	mode := os.FileMode(meta.Mode) & 0o777
	if mode == 0 {
		mode = 0o644
	}

	// Write to a temp file in the same directory, then rename atomically so a
	// mid-stream failure never leaves a truncated target behind.
	tmpRel := filepath.Join(filepath.Dir(rel), fmt.Sprintf(".bean-tmp-%s", randSuffix()))
	f, err := s.root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return nil, fsErr("create temp for "+meta.Path, err)
	}
	committed := false
	defer func() {
		f.Close()
		if !committed {
			_ = s.root.Remove(tmpRel)
		}
	}()

	var written int64
	for {
		frame, rerr := recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, status.Errorf(codes.Internal, "recv: %v", rerr)
		}
		data := frame.GetData()
		if data == nil {
			continue
		}
		n, werr := f.Write(data)
		written += int64(n)
		if werr != nil {
			return nil, fsErr("write "+meta.Path, werr)
		}
	}
	if err := f.Sync(); err != nil {
		return nil, fsErr("sync "+meta.Path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fsErr("close "+meta.Path, err)
	}
	if err := s.root.Rename(tmpRel, rel); err != nil {
		return nil, fsErr("commit "+meta.Path, err)
	}
	committed = true
	return &commonv1.WriteFileResponse{BytesWritten: written}, nil
}

// mkdirAllRoot creates dir and parents inside the sandbox root.
func (s *Server) mkdirAllRoot(dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(filepath.Clean(dir), string(os.PathSeparator))
	cur := ""
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		cur = filepath.Join(cur, p)
		if err := s.root.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) DeleteFile(ctx context.Context, req *commonv1.DeleteFileRequest) (*commonv1.DeleteFileResponse, error) {
	rel, err := s.rootRelative(req.Path)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, status.Error(codes.InvalidArgument, "refusing to delete sandbox root")
	}
	if err := s.root.RemoveAll(rel); err != nil && !os.IsNotExist(err) {
		return nil, fsErr("delete "+req.Path, err)
	}
	return &commonv1.DeleteFileResponse{}, nil
}

func (s *Server) ListDir(ctx context.Context, req *commonv1.ListDirRequest) (*commonv1.ListDirResponse, error) {
	rel, err := s.rootRelative(req.Path)
	if err != nil {
		return nil, err
	}
	d, err := s.root.Open(rel)
	if err != nil {
		return nil, fsErr("opendir "+req.Path, err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fsErr("readdir "+req.Path, err)
	}
	resp := &commonv1.ListDirResponse{}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		resp.Entries = append(resp.Entries, &commonv1.DirEntry{
			Name:      e.Name(),
			Size:      info.Size(),
			Mode:      uint32(info.Mode().Perm()),
			MtimeUnix: info.ModTime().Unix(),
			IsDir:     e.IsDir(),
		})
	}
	return resp, nil
}

func (s *Server) GetLogs(req *commonv1.GetLogsRequest, stream agentv1.AgentService_GetLogsServer) error {
	return s.getLogsTo(req, func(chunk *commonv1.LogChunk) error { return stream.Send(chunk) })
}

// getLogsTo is the transport-independent core of GetLogs, shared by the grpc-go
// method and the Connect adapter.
func (s *Server) getLogsTo(req *commonv1.GetLogsRequest, send func(*commonv1.LogChunk) error) error {
	data := s.logs.Snapshot()
	if req.TailLines > 0 {
		data = tailLines(data, int(req.TailLines))
	}
	if len(data) > 0 {
		if err := send(&commonv1.LogChunk{Data: data}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) StartUserProcess(ctx context.Context, req *agentv1.StartUserProcessRequest) (*agentv1.StartUserProcessResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userProc != nil {
		return nil, status.Error(codes.FailedPrecondition, "user process already started")
	}
	argv := append(append([]string{}, req.Entrypoint...), req.Cmd...)
	if len(argv) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty entrypoint and cmd")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if req.Workdir != "" {
		wd, err := s.hostPath(req.Workdir)
		if err != nil {
			return nil, err
		}
		cmd.Dir = wd
	} else if s.rootDir != "" {
		cmd.Dir = s.rootDir
	}
	cmd.Env = buildEnv(req.Env)
	cmd.Stdout = s.logs
	cmd.Stderr = s.logs
	cmd.SysProcAttr = execSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, status.Errorf(codes.Internal, "start: %v", err)
	}
	s.userProc = cmd.Process
	// Reap and clear state so a crashed user process can be restarted.
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if err != nil {
			exitCode = -1
		}
		fmt.Fprintf(s.logs, "\n[beand] user process exited: code=%d\n", exitCode)
		s.mu.Lock()
		s.userProc = nil
		s.userExitCode = &exitCode
		s.mu.Unlock()
	}()
	return &agentv1.StartUserProcessResponse{Pid: int64(cmd.Process.Pid)}, nil
}

// UserExitCode returns the last user-process exit code, if it has exited.
func (s *Server) UserExitCode() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userExitCode == nil {
		return 0, false
	}
	return *s.userExitCode, true
}

func tailLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	count := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count > n {
				return data[i+1:]
			}
		}
	}
	return data
}
