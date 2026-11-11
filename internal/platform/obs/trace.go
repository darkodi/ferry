package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is Ferry's tracer. Spans are cheap and no-ops until Setup runs.
var Tracer = otel.Tracer("github.com/darkodi/ferry")

// propagator is W3C Trace Context: the traceparent header.
var propagator = propagation.TraceContext{}

// Setup installs the tracer provider for one binary and returns a
// function that flushes and stops it. Spans are always created (so logs
// carry a trace_id and the trace crosses the queue); they are exported
// only when OTEL_EXPORTER_OTLP_ENDPOINT is set, over OTLP/HTTP (Jaeger
// locally, a collector on AWS). The exporter reads the standard OTEL_*
// variables.
func Setup(ctx context.Context, service string) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, fmt.Errorf("obs: trace resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("obs: OTLP exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator)
	return tp.Shutdown, nil
}

// TraceParent returns the W3C traceparent of the span in ctx, or "" if
// there is none. It is what crosses process boundaries: outbox rows and
// queue messages.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceParent returns ctx carrying the remote span described by
// traceparent, so spans started from it continue that trace. A malformed
// or empty value leaves ctx unchanged.
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// LogHandler adds trace_id and span_id to every record logged with a
// context that carries a span, so a log line leads to its trace.
type LogHandler struct{ slog.Handler }

// NewLogHandler wraps h.
func NewLogHandler(h slog.Handler) *LogHandler { return &LogHandler{h} }

// Handle implements slog.Handler.
func (l *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return l.Handler.Handle(ctx, r) //nolint:wrapcheck // a handler passes its inner handler's error through
}

// WithAttrs implements slog.Handler.
func (l *LogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &LogHandler{l.Handler.WithAttrs(as)}
}

// WithGroup implements slog.Handler.
func (l *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{l.Handler.WithGroup(name)}
}

// NewLogger is the JSON logger every binary uses, with trace ids.
func NewLogger() *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
}
