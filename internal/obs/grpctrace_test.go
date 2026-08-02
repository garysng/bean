package obs

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// setupRecorder installs a real tracer provider that records spans in memory,
// so a test can assert on parent/child relationships rather than only on the
// absence of a panic.
func setupRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// TestTraceCrossesGRPC is the property the whole feature rests on: a span
// started by the caller and a span created by the server land in one trace,
// with the server's span a child of the caller's.
//
// Asserting only that both spans exist would pass even if propagation were
// broken, since each process would still produce its own disconnected root.
func TestTraceCrossesGRPC(t *testing.T) {
	rec := setupRecorder(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryServerTrace("test-server")),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(UnaryClientTrace()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, clientSpan := Tracer("test-client").Start(context.Background(), "caller")
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check: %v", err)
	}
	clientSpan.End()

	spans := rec.Ended()
	var caller, server sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.Name() {
		case "caller":
			caller = s
		case "/grpc.health.v1.Health/Check":
			server = s
		}
	}
	if caller == nil {
		t.Fatal("client span not recorded")
	}
	if server == nil {
		t.Fatal("server span not recorded: the interceptor did not start one")
	}
	if got, want := server.SpanContext().TraceID(), caller.SpanContext().TraceID(); got != want {
		t.Errorf("server span is in trace %s, caller in %s: context did not propagate", got, want)
	}
	if got, want := server.Parent().SpanID(), caller.SpanContext().SpanID(); got != want {
		t.Errorf("server span parent = %s, want caller span %s", got, want)
	}
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("server span kind = %v, want server", server.SpanKind())
	}
}

// TestInjectDoesNotMutateCallerMetadata guards a leak that only shows up with
// concurrent calls: if injection wrote into the caller's outgoing metadata map
// instead of a copy, one call's traceparent would attach to its siblings and
// unrelated requests would appear in the same trace.
func TestInjectDoesNotMutateCallerMetadata(t *testing.T) {
	setupRecorder(t)

	base := metadata.Pairs("authorization", "token")
	ctx := metadata.NewOutgoingContext(context.Background(), base)

	ctx1, span1 := Tracer("t").Start(ctx, "first")
	_ = injectTrace(ctx1)
	span1.End()

	if got := base.Get("traceparent"); len(got) != 0 {
		t.Fatalf("injection wrote into the caller's metadata: traceparent=%v", got)
	}
	if got := base.Get("authorization"); len(got) != 1 || got[0] != "token" {
		t.Errorf("existing metadata damaged: %v", got)
	}

	// The second call must carry its own span, not the first one's.
	ctx2, span2 := Tracer("t").Start(ctx, "second")
	out2 := injectTrace(ctx2)
	span2.End()
	md2, _ := metadata.FromOutgoingContext(out2)
	if tp := md2.Get("traceparent"); len(tp) != 1 {
		t.Fatalf("second call has %d traceparent values, want 1", len(tp))
	}
}

// TestNoopProviderWhenDisabled checks that instrumentation is safe to leave in
// place with tracing off: call sites do not guard on it, so a nil-ish provider
// would turn every span into a crash.
func TestNoopProviderWhenDisabled(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), TracingConfig{Service: "x"})
	if err != nil {
		t.Fatalf("setup with empty endpoint: %v", err)
	}
	defer shutdown(context.Background())

	ctx, span := Tracer("x").Start(context.Background(), "s")
	Fail(ctx, context.Canceled)
	span.End()

	if id := TraceIDFrom(ctx); id != "" {
		// A no-op tracer yields an invalid span context, so there is nothing
		// to correlate on — which is the honest answer rather than a fake id.
		t.Errorf("TraceIDFrom on a no-op span = %q, want empty", id)
	}
}

// TestSetupWithEndpointBuildsResource exercises the path an empty endpoint skips.
//
// The first version of this code built its resource with resource.Merge against
// resource.Default(), which errors when the pinned semconv version and the
// SDK's disagree. Every unit test passed because they all left Endpoint empty
// and returned before that line; the failure appeared only as a process that
// exited at startup on a real host.
func TestSetupWithEndpointBuildsResource(t *testing.T) {
	// The exporter connects lazily, so an address with nothing behind it still
	// exercises resource construction and provider setup without a collector.
	shutdown, err := SetupTracing(context.Background(), TracingConfig{
		Endpoint: "127.0.0.1:1", Service: "bean-test", Version: "v0", Insecure: true,
	})
	if err != nil {
		t.Fatalf("setup with an endpoint: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	ctx, span := Tracer("x").Start(context.Background(), "s")
	span.End()
	if TraceIDFrom(ctx) == "" {
		t.Error("no trace id from a configured provider: sampling or setup is wrong")
	}
}
