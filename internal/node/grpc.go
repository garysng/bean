package node

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

// GRPCServer implements nodev1.SandboxServiceServer on top of Manager.
type GRPCServer struct {
	nodev1.UnimplementedSandboxServiceServer
	mgr *Manager
}

func NewGRPCServer(mgr *Manager) *GRPCServer { return &GRPCServer{mgr: mgr} }

// connErr maps AgentConn failures to gRPC codes: unknown sandbox -> NotFound,
// non-runnable state -> FailedPrecondition (docs/api-design.md error table).
func connErr(err error) error {
	if errors.Is(err, ErrSandboxNotFound) {
		return status.Errorf(codes.NotFound, "%v", err)
	}
	return status.Errorf(codes.FailedPrecondition, "%v", err)
}

func (s *GRPCServer) CreateSandbox(ctx context.Context, req *nodev1.CreateSandboxRequest) (*nodev1.CreateSandboxResponse, error) {
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec required")
	}
	sb, err := s.mgr.Create(ctx, req.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create: %v", err)
	}
	return &nodev1.CreateSandboxResponse{Status: &nodev1.SandboxStatus{
		SandboxId: req.Spec.SandboxId,
		State:     string(sb.State),
	}}, nil
}

func (s *GRPCServer) DestroySandbox(ctx context.Context, req *nodev1.DestroySandboxRequest) (*nodev1.DestroySandboxResponse, error) {
	if err := s.mgr.Destroy(ctx, req.SandboxId, req.Force); err != nil {
		return nil, status.Errorf(codes.Internal, "destroy: %v", err)
	}
	return &nodev1.DestroySandboxResponse{}, nil
}

func (s *GRPCServer) PauseSandbox(ctx context.Context, req *nodev1.PauseSandboxRequest) (*nodev1.PauseSandboxResponse, error) {
	if err := s.mgr.Pause(ctx, req.SandboxId); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "pause: %v", err)
	}
	return &nodev1.PauseSandboxResponse{}, nil
}

func (s *GRPCServer) ResumeSandbox(ctx context.Context, req *nodev1.ResumeSandboxRequest) (*nodev1.ResumeSandboxResponse, error) {
	if err := s.mgr.Resume(ctx, req.SandboxId); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "resume: %v", err)
	}
	return &nodev1.ResumeSandboxResponse{}, nil
}

func (s *GRPCServer) GetSandbox(ctx context.Context, req *nodev1.GetSandboxRequest) (*nodev1.GetSandboxResponse, error) {
	st := s.mgr.StatusOf(req.SandboxId)
	if st == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.SandboxId)
	}
	return &nodev1.GetSandboxResponse{Status: st}, nil
}

func (s *GRPCServer) StartUserProcess(ctx context.Context, req *nodev1.StartUserProcessNodeRequest) (*nodev1.StartUserProcessNodeResponse, error) {
	spec := s.mgr.SpecOf(req.SandboxId)
	if spec == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.SandboxId)
	}
	conn, release, err := s.mgr.AgentConn(ctx, req.SandboxId)
	if err != nil {
		return nil, connErr(err)
	}
	defer release()
	resp, err := agentv1.NewAgentServiceClient(conn).StartUserProcess(ctx, &agentv1.StartUserProcessRequest{
		Cmd: spec.Cmd, Env: spec.Env,
	})
	if err != nil {
		return nil, err
	}
	return &nodev1.StartUserProcessNodeResponse{Pid: resp.Pid}, nil
}

// PrewarmImage makes an image usable on this node ahead of any sandbox.
//
// The call blocks until the image is ready, so the control plane learns the
// outcome rather than polling. A first pull can take minutes — longer than a
// create should block — which is why warming is a separate operation.
func (s *GRPCServer) PrewarmImage(ctx context.Context, req *nodev1.PrewarmImageRequest) (*nodev1.PrewarmImageResponse, error) {
	if req.Image == "" {
		return nil, status.Error(codes.InvalidArgument, "image required")
	}
	if err := s.mgr.PrewarmImage(ctx, req.Image); err != nil {
		return nil, status.Errorf(codes.Internal, "prewarm %s: %v", req.Image, err)
	}
	return &nodev1.PrewarmImageResponse{Ready: true}, nil
}

// ---- data plane passthrough ----

func (s *GRPCServer) Exec(ctx context.Context, req *commonv1.ExecRequest) (*commonv1.ExecResponse, error) {
	conn, release, err := s.mgr.AgentConn(ctx, req.SandboxId)
	if err != nil {
		return nil, connErr(err)
	}
	defer release()
	return agentv1.NewAgentServiceClient(conn).Exec(ctx, req)
}

func (s *GRPCServer) ReadFile(req *commonv1.ReadFileRequest, stream nodev1.SandboxService_ReadFileServer) error {
	conn, release, err := s.mgr.AgentConn(stream.Context(), req.SandboxId)
	if err != nil {
		return connErr(err)
	}
	defer release()
	up, err := agentv1.NewAgentServiceClient(conn).ReadFile(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		chunk, rerr := up.Recv()
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if serr := stream.Send(chunk); serr != nil {
			return serr
		}
	}
}

func (s *GRPCServer) WriteFile(stream nodev1.SandboxService_WriteFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "recv meta: %v", err)
	}
	meta := first.GetMeta()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "first frame must be meta")
	}
	conn, release, err := s.mgr.AgentConn(stream.Context(), meta.SandboxId)
	if err != nil {
		return connErr(err)
	}
	defer release()
	up, err := agentv1.NewAgentServiceClient(conn).WriteFile(stream.Context())
	if err != nil {
		return err
	}
	if err := up.Send(first); err != nil {
		return err
	}
	for {
		frame, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
		if serr := up.Send(frame); serr != nil {
			return serr
		}
	}
	resp, err := up.CloseAndRecv()
	if err != nil {
		return err
	}
	return stream.SendAndClose(resp)
}

func (s *GRPCServer) DeleteFile(ctx context.Context, req *commonv1.DeleteFileRequest) (*commonv1.DeleteFileResponse, error) {
	conn, release, err := s.mgr.AgentConn(ctx, req.SandboxId)
	if err != nil {
		return nil, connErr(err)
	}
	defer release()
	return agentv1.NewAgentServiceClient(conn).DeleteFile(ctx, req)
}

func (s *GRPCServer) ListDir(ctx context.Context, req *commonv1.ListDirRequest) (*commonv1.ListDirResponse, error) {
	conn, release, err := s.mgr.AgentConn(ctx, req.SandboxId)
	if err != nil {
		return nil, connErr(err)
	}
	defer release()
	return agentv1.NewAgentServiceClient(conn).ListDir(ctx, req)
}

func (s *GRPCServer) GetLogs(req *commonv1.GetLogsRequest, stream nodev1.SandboxService_GetLogsServer) error {
	conn, release, err := s.mgr.AgentConn(stream.Context(), req.SandboxId)
	if err != nil {
		return connErr(err)
	}
	defer release()
	up, err := agentv1.NewAgentServiceClient(conn).GetLogs(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		chunk, rerr := up.Recv()
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if serr := stream.Send(chunk); serr != nil {
			return serr
		}
	}
}

// snapshotChunkSize bounds gRPC frame size while keeping the stream
// efficient for multi-hundred-megabyte checkpoints.
const snapshotChunkSize = 1 << 20

// SnapshotSandbox streams a checkpoint to the caller. The writer adapter
// turns Manager's io.Writer contract into gRPC frames.
func (s *GRPCServer) SnapshotSandbox(req *nodev1.SnapshotSandboxRequest,
	stream nodev1.SandboxService_SnapshotSandboxServer) error {
	if req.GetSandboxId() == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id required")
	}
	w := &chunkWriter{stream: stream}
	if err := s.mgr.Snapshot(stream.Context(), req.SandboxId, w); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			return status.Errorf(codes.NotFound, "%v", err)
		}
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return nil
}

// chunkWriter splits writes into gRPC frames.
type chunkWriter struct {
	stream nodev1.SandboxService_SnapshotSandboxServer
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), snapshotChunkSize)
		if err := c.stream.Send(&nodev1.SnapshotChunk{Data: p[:n]}); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// RestoreSandbox consumes a spec frame followed by checkpoint data. The
// checkpoint is piped straight into the runtime so a large restore does not
// have to be buffered in memory.
func (s *GRPCServer) RestoreSandbox(stream nodev1.SandboxService_RestoreSandboxServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "recv spec frame: %v", err)
	}
	spec := first.GetSpec()
	if spec == nil {
		return status.Error(codes.InvalidArgument, "first frame must carry the spec")
	}

	pr, pw := io.Pipe()
	var restored atomic.Int64
	go func() {
		for {
			frame, rerr := stream.Recv()
			if rerr == io.EOF {
				pw.Close()
				return
			}
			if rerr != nil {
				pw.CloseWithError(rerr)
				return
			}
			data := frame.GetData()
			if len(data) == 0 {
				continue
			}
			if _, werr := pw.Write(data); werr != nil {
				pw.CloseWithError(werr)
				return
			}
			restored.Add(int64(len(data)))
		}
	}()

	sb, err := s.mgr.RestoreSandbox(stream.Context(), spec, pr)
	// Draining matters: an early runtime failure would otherwise leave the
	// sender blocked on a full pipe.
	pr.CloseWithError(err)
	if err != nil {
		return status.Errorf(codes.Internal, "restore: %v", err)
	}
	return stream.SendAndClose(&nodev1.RestoreSandboxResponse{
		Status: &nodev1.SandboxStatus{
			SandboxId: spec.SandboxId,
			State:     string(sb.State),
		},
		BytesRestored: restored.Load(),
	})
}
