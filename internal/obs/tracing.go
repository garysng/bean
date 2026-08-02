// Tracing configures distributed tracing.
//
// Metrics answer "how slow is create"; a trace answers "slow where". A create
// crosses three processes — gateway, node, and the agent inside the guest — and
// the interesting latency is usually in one of the handoffs. Correlating that
// from logs requires reading three files and doing arithmetic by hand, which is
// why the request id alone was not enough.
package obs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Attribute keys shared across components, for the same reason the log keys
// are shared: a span attribute only groups if every process spells it alike.
const (
	AttrSandbox  = "bean.sandbox"
	AttrNode     = "bean.node"
	AttrSnapshot = "bean.snapshot"
	AttrImage    = "bean.image"
	AttrPhase    = "bean.phase"
)

// TracingConfig describes where spans go.
type TracingConfig struct {
	// Endpoint is an OTLP/gRPC collector, e.g. "localhost:4317". Empty
	// disables tracing entirely.
	Endpoint string
	// Service names this process in the trace, e.g. "bean-api".
	Service string
	// Version is reported as the service version.
	Version string
	// Insecure skips TLS to the collector. Collectors are normally reached
	// over a node-local or in-cluster hop, so this defaults on in practice.
	Insecure bool
	// SampleRatio is the head sampling ratio for root spans. Zero means
	// always sample: a platform doing a few creates a second is not where
	// trace volume becomes a problem, and a missing trace for the one slow
	// create is exactly the trace worth having.
	SampleRatio float64
}

// SetupTracing installs the global tracer provider and propagator, returning a
// shutdown function.
//
// A disabled or unreachable collector must not stop the process: tracing is
// diagnostic, and a node that refuses to start because an observability
// endpoint moved is a worse outage than the one it was meant to diagnose. When
// Endpoint is empty this installs a no-op provider, so instrumentation at call
// sites stays unconditional and does not need to check whether tracing is on.
func SetupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	// The propagator is set even when tracing is off. An incoming traceparent
	// then still round-trips through this process to the next one, so turning
	// tracing off in the middle of a chain leaves a gap rather than severing
	// the trace into two unrelated halves.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	// resource.New merges with the SDK's defaults itself and adopts the SDK's
	// schema URL. Building the resource by hand and calling resource.Merge
	// against resource.Default() instead fails outright whenever the pinned
	// semconv version differs from the SDK's — which is a normal consequence of
	// upgrading one and not the other, and it surfaced as a process that would
	// not start rather than as a compile error.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.Service),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	sampler := sdktrace.AlwaysSample()
	if cfg.SampleRatio > 0 && cfg.SampleRatio < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Tracer returns the tracer for a component.
func Tracer(name string) trace.Tracer {
	return otel.Tracer("github.com/garysng/bean/" + name)
}

// TraceIDFrom returns the trace id in a context as a hex string, or "".
//
// This is what ties a trace to the logs around it: the log line carries the
// same id the collector indexes the span under, so finding the logs for a slow
// span does not require a second correlation scheme.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Fail records an error on the span in ctx, if any.
//
// Recording the error and setting the status are separate calls in the OTel
// API and it is easy to do only the first, which leaves the span green with an
// exception event nobody filters on.
func Fail(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Phase annotates a span with the create phase it covers, matching the phase
// label already used by the node's duration histogram so a span and a metric
// bucket can be compared without a translation table.
func Phase(name string) attribute.KeyValue {
	return attribute.String(AttrPhase, name)
}
