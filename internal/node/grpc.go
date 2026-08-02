package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
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
		// A node declining work because it is low on disk is not a failure of this
		// request: the same spec would succeed on another node, and will succeed here
		// once space is reclaimed. ResourceExhausted says that, where Internal would
		// tell the caller to report a bug.
		var pressure *ErrDiskPressure
		if errors.As(err, &pressure) {
			return nil, status.Errorf(codes.ResourceExhausted, "create: %v", err)
		}
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

// CommitSandbox seals a sandbox's filesystem into a base image.
func (s *GRPCServer) CommitSandbox(ctx context.Context, req *nodev1.CommitSandboxRequest) (*nodev1.CommitSandboxResponse, error) {
	if req.Tag == "" {
		return nil, status.Error(codes.InvalidArgument, "tag required")
	}
	if err := s.mgr.CommitSandbox(ctx, req.SandboxId, req.Tag); err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			return nil, status.Errorf(codes.NotFound, "sandbox %s not found", req.SandboxId)
		}
		return nil, status.Errorf(codes.Internal, "commit %s: %v", req.SandboxId, err)
	}
	return &nodev1.CommitSandboxResponse{ImageRef: req.Tag}, nil
}

// BuildImage builds a base image from a Dockerfile on this node.
//
// The call blocks for the build's duration, which can be minutes. That is the
// right shape here: the control plane runs it in the background and reports
// progress, so an intermediate polling protocol between the two would add a
// state machine without adding information.
func (s *GRPCServer) BuildImage(ctx context.Context, req *nodev1.BuildImageRequest) (*nodev1.BuildImageResponse, error) {
	if req.Tag == "" {
		return nil, status.Error(codes.InvalidArgument, "tag required")
	}
	if req.Dockerfile == "" {
		return nil, status.Error(codes.InvalidArgument, "dockerfile required")
	}
	ref, err := s.mgr.BuildImage(ctx, runtime.BuildRequest{
		Tag:        req.Tag,
		Dockerfile: req.Dockerfile,
		ContextTar: req.ContextTar,
		BuildArgs:  req.BuildArgs,
		SizeMiB:    req.SizeMib,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build %s: %v", req.Tag, err)
	}
	return &nodev1.BuildImageResponse{ImageRef: ref}, nil
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
	opts := runtime.CheckpointOptions{
		IncludeMemory: req.GetIncludeMemory(),
		Diff:          req.GetDiff(),
	}
	if err := s.mgr.Snapshot(stream.Context(), req.SandboxId, w, opts); err != nil {
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

	layers, restored, closeLayers, err := recvLayers(stream, spec)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}

	sb, err := s.mgr.RestoreSandbox(stream.Context(), spec, layers)
	// Closing matters: an early runtime failure would otherwise leave the sender
	// blocked writing into a pipe nobody reads.
	closeLayers(err)
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

// maxRestoreLayers bounds a chain so a malformed or hostile spec cannot make the
// node allocate pipes without limit. It is well above the depth the control plane
// allows, so reaching it means the spec is wrong rather than deep.
const maxRestoreLayers = 64

// recvLayers turns the frame stream into one reader per declared layer.
//
// Each layer needs its own reader: a layer is a gzip stream, and a reader handed
// the concatenation of several would stop at the first one's end and leave the
// rest unread. The spec declares the chain, so the readers exist before any
// reading starts; layer_end frames say where one bundle stops and the next
// begins.
//
// The pipes are unbuffered, so the sender's pace follows the node's consumption
// and a slow merge never accumulates the chain in memory. That also means every
// layer must be consumed — the returned close function unblocks the sender when
// the restore ends early.
func recvLayers(stream nodev1.SandboxService_RestoreSandboxServer, spec *nodev1.SandboxSpec) (
	[]runtime.SnapshotLayer, *atomic.Int64, func(error), error) {

	ids := spec.GetSnapshotChain()
	if len(ids) == 0 {
		// No chain declared: the stream is one self-contained bundle. Its id may
		// also be empty, which means "do not cache" rather than "no layer".
		ids = []string{spec.GetSnapshotId()}
	}
	if len(ids) > maxRestoreLayers {
		return nil, nil, nil, fmt.Errorf("snapshot chain has %d layers, more than the %d supported",
			len(ids), maxRestoreLayers)
	}

	layers := make([]runtime.SnapshotLayer, len(ids))
	writers := make([]*io.PipeWriter, len(ids))
	for i, id := range ids {
		pr, pw := io.Pipe()
		layers[i] = runtime.SnapshotLayer{ID: id, Data: pr}
		writers[i] = pw
	}

	var restored atomic.Int64
	closeAll := func(err error) {
		for _, w := range writers {
			if err != nil {
				w.CloseWithError(err)
				continue
			}
			w.Close()
		}
	}

	go func() {
		at := 0
		for {
			frame, err := stream.Recv()
			if err == io.EOF {
				// Closing every remaining writer, not just the current one: a
				// truncated stream must surface as a short layer rather than
				// leaving a later layer's reader blocked forever.
				closeAll(nil)
				return
			}
			if err != nil {
				closeAll(err)
				return
			}
			if frame.GetLayerEnd() {
				// The boundary closes this layer so its reader sees a clean end of
				// stream; without that the gzip reader would wait for more.
				writers[at].Close()
				at++
				if at >= len(writers) {
					// More boundaries than the spec declared layers. Folding the
					// extra bytes into the last layer would merge two checkpoints,
					// so this fails instead.
					closeAll(fmt.Errorf("restore stream carries more layers than the %d declared",
						len(writers)))
					return
				}
				continue
			}
			data := frame.GetData()
			if len(data) == 0 {
				continue
			}
			if _, err := writers[at].Write(data); err != nil {
				closeAll(err)
				return
			}
			restored.Add(int64(len(data)))
		}
	}()

	return layers, &restored, closeAll, nil
}
