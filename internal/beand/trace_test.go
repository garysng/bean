package beand

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"

	"github.com/garysng/bean/internal/logging"
)

// TestAdoptsCallerTraceID is the agent's whole contribution to tracing: it
// cannot export spans, but its logs must carry the caller's trace id so a slow
// span on the node can be joined to what the agent was doing.
func TestAdoptsCallerTraceID(t *testing.T) {
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	md := metadata.Pairs("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	ctx := withCallerTrace(metadata.NewIncomingContext(context.Background(), md))

	if got := logging.RequestFrom(ctx); got != traceID {
		t.Errorf("request id = %q, want the caller's trace id %q", got, traceID)
	}
}

// TestNoTraceparentLeavesContextAlone checks the path taken by a caller that is
// not tracing: the agent must not invent an id, since a fabricated one would
// group unrelated requests under the same key.
func TestNoTraceparentLeavesContextAlone(t *testing.T) {
	ctx := withCallerTrace(metadata.NewIncomingContext(context.Background(), metadata.MD{}))
	if got := logging.RequestFrom(ctx); got != "" {
		t.Errorf("request id = %q, want empty when the caller sent no traceparent", got)
	}

	// No metadata at all is the local/unix-socket dev path.
	if got := logging.RequestFrom(withCallerTrace(context.Background())); got != "" {
		t.Errorf("request id = %q with no metadata, want empty", got)
	}
}

// TestMalformedTraceparentIgnored guards against a corrupt header producing a
// half-valid span context that would then be logged as a real id.
func TestMalformedTraceparentIgnored(t *testing.T) {
	for _, tp := range []string{"", "garbage", "00-tooshort-01", "99-xx-yy-zz"} {
		md := metadata.Pairs("traceparent", tp)
		ctx := withCallerTrace(metadata.NewIncomingContext(context.Background(), md))
		if got := logging.RequestFrom(ctx); got != "" {
			t.Errorf("traceparent %q yielded request id %q, want empty", tp, got)
		}
	}
}

// TestInterceptorPassesContextThrough makes sure the handler actually receives
// the augmented context. An interceptor that computes the id and then calls the
// handler with the original context would pass the tests above and still leave
// every log line without an id.
func TestInterceptorPassesContextThrough(t *testing.T) {
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	md := metadata.Pairs("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	var seen string
	_, err := UnaryTraceLogging()(ctx, nil, nil, func(ctx context.Context, _ any) (any, error) {
		seen = logging.RequestFrom(ctx)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != traceID {
		t.Errorf("handler saw request id %q, want %q", seen, traceID)
	}
}

// TestSpanContextNotStarted documents that the agent only reads the id and does
// not become a span participant: no exporter exists in the guest, so a started
// span would be recorded nowhere and its cost paid for nothing.
func TestSpanContextNotStarted(t *testing.T) {
	md := metadata.Pairs("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := withCallerTrace(metadata.NewIncomingContext(context.Background(), md))
	if trace.SpanFromContext(ctx).IsRecording() {
		t.Error("agent started a recording span; it has no exporter to send it to")
	}
}
