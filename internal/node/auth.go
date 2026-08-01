package node

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// MetadataTokenKey carries the node token on control-plane -> noded calls.
const MetadataTokenKey = "bean-node-token"

// TokenAuth returns unary/stream interceptors enforcing a shared node token.
// An empty token disables enforcement (loopback dev only); production
// deployments must set one since noded has no other caller authentication.
func TokenAuth(token string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	check := func(ctx context.Context) error {
		if token == "" {
			return nil
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing metadata")
		}
		for _, v := range md.Get(MetadataTokenKey) {
			if subtle.ConstantTimeCompare([]byte(v), []byte(token)) == 1 {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "invalid node token")
	}
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {
		if err := check(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
	return unary, stream
}

// WithToken attaches the node token to an outgoing context.
func WithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, MetadataTokenKey, token)
}

// TokenClientInterceptors returns client interceptors that attach the token.
func TokenClientInterceptors(token string) (grpc.UnaryClientInterceptor, grpc.StreamClientInterceptor) {
	unary := func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(WithToken(ctx, token), method, req, reply, cc, opts...)
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(WithToken(ctx, token), desc, cc, method, opts...)
	}
	return unary, stream
}
