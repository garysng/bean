// Package agent implements the in-sandbox bean-agent gRPC service.
package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
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
	logs    *RingBuffer

	mu       sync.Mutex
	userProc *os.Process
}

func NewServer(version, rootDir string) *Server {
	return &Server{
		version:   version,
		startedAt: time.Now(),
		rootDir:   rootDir,
		logs:      NewRingBuffer(8 << 20),
	}
}

// Logs exposes the user-process log buffer (writer side used by process manager).
func (s *Server) Logs() *RingBuffer { return s.logs }

func (s *Server) Health(ctx context.Context, _ *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return &agentv1.HealthResponse{
		AgentVersion:  s.version,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}, nil
}

// resolvePath confines a sandbox-visible path under rootDir and rejects escapes.
func (s *Server) resolvePath(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", status.Error(codes.InvalidArgument, "path must be absolute")
	}
	cleaned := filepath.Clean(p)
	if s.rootDir == "" {
		return cleaned, nil
	}
	joined := filepath.Join(s.rootDir, cleaned)
	root := filepath.Clean(s.rootDir) + string(os.PathSeparator)
	if joined != filepath.Clean(s.rootDir) && !bytes.HasPrefix([]byte(joined+string(os.PathSeparator)), []byte(root)) {
		return "", status.Error(codes.InvalidArgument, "path escapes sandbox root")
	}
	return joined, nil
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
		cwd, err := s.resolvePath(req.Cwd)
		if err != nil {
			return nil, err
		}
		cmd.Dir = cwd
	} else if s.rootDir != "" {
		cmd.Dir = s.rootDir
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	stdout := newCappedBuffer(maxOut)
	stderr := newCappedBuffer(maxOut)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

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
	path, err := s.resolvePath(req.Path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return status.Errorf(codes.NotFound, "file not found: %s", req.Path)
		}
		return status.Errorf(codes.Internal, "open: %v", err)
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&commonv1.FileChunk{Data: buf[:n]}); serr != nil {
				return serr
			}
		}
		if rerr != nil {
			if rerr.Error() == "EOF" {
				return nil
			}
			return status.Errorf(codes.Internal, "read: %v", rerr)
		}
	}
}

func (s *Server) WriteFile(stream agentv1.AgentService_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "missing meta frame: %v", err)
	}
	meta := first.GetMeta()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "first frame must be meta")
	}
	path, err := s.resolvePath(meta.Path)
	if err != nil {
		return err
	}
	if meta.Mkdirs {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return status.Errorf(codes.Internal, "mkdirs: %v", err)
		}
	}
	mode := os.FileMode(meta.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return status.Errorf(codes.Internal, "open: %v", err)
	}
	defer f.Close()

	var written int64
	for {
		frame, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, errEOF()) || rerr.Error() == "EOF" {
				break
			}
			return status.Errorf(codes.Internal, "recv: %v", rerr)
		}
		data := frame.GetData()
		if data == nil {
			continue
		}
		n, werr := f.Write(data)
		written += int64(n)
		if werr != nil {
			return status.Errorf(codes.Internal, "write: %v", werr)
		}
	}
	return stream.SendAndClose(&commonv1.WriteFileResponse{BytesWritten: written})
}

func (s *Server) DeleteFile(ctx context.Context, req *commonv1.DeleteFileRequest) (*commonv1.DeleteFileResponse, error) {
	path, err := s.resolvePath(req.Path)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(path); err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &commonv1.DeleteFileResponse{}, nil
}

func (s *Server) ListDir(ctx context.Context, req *commonv1.ListDirRequest) (*commonv1.ListDirResponse, error) {
	path, err := s.resolvePath(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "dir not found: %s", req.Path)
		}
		return nil, status.Errorf(codes.Internal, "readdir: %v", err)
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
	data := s.logs.Snapshot()
	if req.TailLines > 0 {
		data = tailLines(data, int(req.TailLines))
	}
	if len(data) > 0 {
		if err := stream.Send(&commonv1.LogChunk{Data: data}); err != nil {
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
		wd, err := s.resolvePath(req.Workdir)
		if err != nil {
			return nil, err
		}
		cmd.Dir = wd
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = s.logs
	cmd.Stderr = s.logs
	if err := cmd.Start(); err != nil {
		return nil, status.Errorf(codes.Internal, "start: %v", err)
	}
	s.userProc = cmd.Process
	go func() { _ = cmd.Wait() }()
	return &agentv1.StartUserProcessResponse{Pid: int64(cmd.Process.Pid)}, nil
}

func errEOF() error { return errEOFSentinel }

var errEOFSentinel = errors.New("EOF")

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
