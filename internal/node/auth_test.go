package node

import (
	"context"
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

func startAuthServer(t *testing.T, token string) nodev1.SandboxServiceClient {
	t.Helper()
	mgr := NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unary, stream := TokenAuth(token)
	srv := grpc.NewServer(grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(stream))
	nodev1.RegisterSandboxServiceServer(srv, NewGRPCServer(mgr))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodev1.NewSandboxServiceClient(conn)
}

func TestTokenAuthRejectsMissingToken(t *testing.T) {
	c := startAuthServer(t, "secret-token")
	_, err := c.GetSandbox(context.Background(), &nodev1.GetSandboxRequest{SandboxId: "x"})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestTokenAuthRejectsWrongToken(t *testing.T) {
	c := startAuthServer(t, "secret-token")
	ctx := WithToken(context.Background(), "nope")
	_, err := c.GetSandbox(ctx, &nodev1.GetSandboxRequest{SandboxId: "x"})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestTokenAuthAcceptsValidToken(t *testing.T) {
	c := startAuthServer(t, "secret-token")
	ctx := WithToken(context.Background(), "secret-token")
	_, err := c.GetSandbox(ctx, &nodev1.GetSandboxRequest{SandboxId: "missing"})
	// Passes auth, then legitimately reports NotFound.
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestTokenAuthDisabledWhenEmpty(t *testing.T) {
	c := startAuthServer(t, "")
	_, err := c.GetSandbox(context.Background(), &nodev1.GetSandboxRequest{SandboxId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestTokenClientInterceptorsAttachToken(t *testing.T) {
	// Server enforces a token; the client interceptors must supply it.
	mgr := NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unary, stream := TokenAuth("tok-123")
	srv := grpc.NewServer(grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(stream))
	nodev1.RegisterSandboxServiceServer(srv, NewGRPCServer(mgr))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	cu, cs := TokenClientInterceptors("tok-123")
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(cu), grpc.WithStreamInterceptor(cs))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := nodev1.NewSandboxServiceClient(conn)

	// Unary passes auth (NotFound proves it reached the handler).
	if _, err := c.GetSandbox(context.Background(), &nodev1.GetSandboxRequest{SandboxId: "x"}); status.Code(err) != codes.NotFound {
		t.Errorf("unary: err = %v, want NotFound", err)
	}
	// Stream passes auth too.
	st, err := c.GetLogs(context.Background(), &commonv1.GetLogsRequest{SandboxId: "x"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Errorf("stream: err = %v, want NotFound", err)
	}
}

func TestWithTokenEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	if got := WithToken(ctx, ""); got != ctx {
		t.Error("empty token should return the original context")
	}
}
