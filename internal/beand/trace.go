package beand

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/garysng/bean/internal/logging"
)

// The agent extracts trace context but never exports spans, and deliberately
// does not import the tracing SDK.
//
// It has no route to a collector: the guest's only channel is an inbound vsock
// connection from the node. Nor should it grow one — the agent ships on a disk
// image that is attached to every microVM, so its size is paid per boot, and
// the SDK plus an OTLP exporter is a large addition for telemetry that cannot
// leave the guest anyway.
//
// What it can do is adopt the caller's trace id for its own log lines. That
// turns "the slow part was inside the guest" from a guess into something a
// reader can confirm: the node's span and the agent's logs carry the same id.
var agentPropagator = propagation.TraceContext{}

// metadataCarrier reads trace headers out of incoming gRPC metadata.
type metadataCarrier metadata.MD

func (c metadataCarrier) Get(key string) string {
	v := metadata.MD(c).Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (c metadataCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// UnaryTraceLogging adopts the caller's trace id as this call's request id.
func UnaryTraceLogging() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(withCallerTrace(ctx), req)
	}
}

// StreamTraceLogging is the streaming counterpart.
func StreamTraceLogging() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, tracedStream{ServerStream: ss, ctx: withCallerTrace(ss.Context())})
	}
}

type tracedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s tracedStream) Context() context.Context { return s.ctx }

func withCallerTrace(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	sc := trace.SpanContextFromContext(agentPropagator.Extract(ctx, metadataCarrier(md)))
	if !sc.HasTraceID() {
		return ctx
	}
	return logging.WithRequest(ctx, sc.TraceID().String())
}
