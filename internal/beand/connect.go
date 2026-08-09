package beand

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	"github.com/garysng/bean/internal/gen/bean/agent/v1/agentv1connect"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
)

// ConnectServer adapts the agent's logic to a Connect handler, so the same
// implementation serves the Connect protocol, gRPC, and gRPC-Web from one set of
// handlers on one port. noded's control path keeps dialling as a gRPC client
// (Connect servers accept it unchanged); the data-plane client and the SDK reach
// the same methods over HTTP/JSON.
//
// The unary methods delegate straight to *Server. The streaming methods delegate
// to the transport-independent cores (readFileTo / writeFileFrom / getLogsTo),
// which is why the file-safety logic is not duplicated here.
type ConnectServer struct {
	svc *Server
}

// NewConnectServer wraps an existing agent Server as a Connect handler.
func NewConnectServer(svc *Server) *ConnectServer { return &ConnectServer{svc: svc} }

var _ agentv1connect.AgentServiceHandler = (*ConnectServer)(nil)

func (c *ConnectServer) Health(ctx context.Context, req *connect.Request[agentv1.HealthRequest]) (*connect.Response[agentv1.HealthResponse], error) {
	resp, err := c.svc.Health(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) Exec(ctx context.Context, req *connect.Request[commonv1.ExecRequest]) (*connect.Response[commonv1.ExecResponse], error) {
	resp, err := c.svc.Exec(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) DeleteFile(ctx context.Context, req *connect.Request[commonv1.DeleteFileRequest]) (*connect.Response[commonv1.DeleteFileResponse], error) {
	resp, err := c.svc.DeleteFile(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) ListDir(ctx context.Context, req *connect.Request[commonv1.ListDirRequest]) (*connect.Response[commonv1.ListDirResponse], error) {
	resp, err := c.svc.ListDir(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) StartUserProcess(ctx context.Context, req *connect.Request[agentv1.StartUserProcessRequest]) (*connect.Response[agentv1.StartUserProcessResponse], error) {
	resp, err := c.svc.StartUserProcess(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) ReadFile(ctx context.Context, req *connect.Request[commonv1.ReadFileRequest], stream *connect.ServerStream[commonv1.FileChunk]) error {
	return c.svc.readFileTo(req.Msg, stream.Send)
}

func (c *ConnectServer) WriteFile(ctx context.Context, stream *connect.ClientStream[commonv1.WriteFileFrame]) (*connect.Response[commonv1.WriteFileResponse], error) {
	// Adapt Connect's Receive()/Msg()/Err() pull model to the recv closure the
	// core expects, reporting a clean end-of-stream as io.EOF exactly as grpc-go
	// does, so writeFileFrom's loop terminates the same way on both transports.
	recv := func() (*commonv1.WriteFileFrame, error) {
		if !stream.Receive() {
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		return stream.Msg(), nil
	}
	resp, err := c.svc.writeFileFrom(recv)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) GetLogs(ctx context.Context, req *connect.Request[commonv1.GetLogsRequest], stream *connect.ServerStream[commonv1.LogChunk]) error {
	return c.svc.getLogsTo(req.Msg, stream.Send)
}

// StreamExec is not implemented, matching the gRPC server (which relies on the
// Unimplemented embed). Returned as a Connect unimplemented error so a caller
// gets the same answer on either transport.
func (c *ConnectServer) StreamExec(ctx context.Context, stream *connect.BidiStream[commonv1.StreamExecFrame, commonv1.StreamExecFrame]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("StreamExec is not implemented"))
}
