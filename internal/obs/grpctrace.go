package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// metadataCarrier adapts gRPC metadata to the propagator's carrier interface.
//
// gRPC metadata keys are lowercased by the transport, and the propagator writes
// "traceparent" which is already lowercase, so no normalisation is needed here.
// Reads go through MD.Get, which lowercases the lookup itself.
type metadataCarrier metadata.MD

func (c metadataCarrier) Get(key string) string {
	v := metadata.MD(c).Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (c metadataCarrier) Set(key, value string) {
	metadata.MD(c).Set(key, value)
}

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// UnaryServerTrace continues an incoming trace and opens a span per call.
//
// The span is a child of the caller's when a traceparent arrives, which is what
// makes a create one tree rather than one disconnected span per process.
func UnaryServerTrace(component string) grpc.UnaryServerInterceptor {
	tracer := Tracer(component)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(md))
		ctx, span := tracer.Start(ctx, info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("rpc.method", info.FullMethod)),
		)
		defer span.End()

		resp, err := handler(ctx, req)
		Fail(ctx, err)
		return resp, err
	}
}

// StreamServerTrace is the streaming counterpart.
//
// The span covers the whole stream, so a long-lived exec shows up as one span
// whose duration is the session — not as a gap in the trace.
func StreamServerTrace(component string) grpc.StreamServerInterceptor {
	tracer := Tracer(component)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		ctx := otel.GetTextMapPropagator().Extract(ss.Context(), metadataCarrier(md))
		ctx, span := tracer.Start(ctx, info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("rpc.method", info.FullMethod)),
		)
		defer span.End()

		err := handler(srv, tracedStream{ServerStream: ss, ctx: ctx})
		Fail(ctx, err)
		return err
	}
}

// tracedStream overrides Context so handlers see the span-carrying context.
// Without this the handler's own spans would attach to the trace root instead
// of to the stream's span, and nested work inside a stream would look unrelated.
type tracedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s tracedStream) Context() context.Context { return s.ctx }

// UnaryClientTrace injects the current trace into outgoing calls.
func UnaryClientTrace() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = injectTrace(ctx)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientTrace is the streaming counterpart.
func StreamClientTrace() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = injectTrace(ctx)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// injectTrace copies the context's trace into outgoing metadata.
//
// The existing metadata is copied rather than mutated: outgoing metadata is
// shared with the caller's context, and writing into it would leak this call's
// traceparent into sibling calls made from the same context.
func injectTrace(ctx context.Context) context.Context {
	md, ok := metadata.FromOutgoingContext(ctx)
	if ok {
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	otel.GetTextMapPropagator().Inject(ctx, metadataCarrier(md))
	return metadata.NewOutgoingContext(ctx, md)
}

// HTTPExtract returns a context continuing the trace described by a request's
// headers, for the gateway's REST edge.
func HTTPExtract(ctx context.Context, header propagation.HeaderCarrier) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, header)
}
