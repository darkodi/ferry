package obs

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/darkodi/ferry/internal/rail"
)

// InstrumentRail wraps a rail so every call records a span, its latency
// and its outcome. The adapter itself stays free of metrics code.
func InstrumentRail(r rail.Rail) rail.Rail { return instrumented{r} }

type instrumented struct{ rail.Rail }

func observe[T any](ctx context.Context, rl rail.Rail, op string, attrs []attribute.KeyValue, call func(context.Context) (T, error)) (T, error) {
	ctx, span := Tracer.Start(ctx, "rail."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(attrs, attribute.String("rail", rl.Name()))...))
	defer span.End()
	start := time.Now()
	out, err := call(ctx)
	RailDuration.WithLabelValues(rl.Name(), op).Observe(time.Since(start).Seconds())
	outcome := "ok"
	switch {
	case errors.Is(err, rail.ErrRejected):
		outcome = "rejected"
	case errors.Is(err, rail.ErrNotFound):
		outcome = "not_found"
	case err != nil:
		outcome = "error"
	}
	RailRequests.WithLabelValues(rl.Name(), op, outcome).Inc()
	span.SetAttributes(attribute.String("outcome", outcome))
	if err != nil && outcome == "error" {
		span.SetStatus(codes.Error, err.Error())
	}
	return out, err
}

func (i instrumented) Prepare(ctx context.Context, req rail.QuoteRequest) (rail.Quote, error) {
	return observe(ctx, i.Rail, "prepare", []attribute.KeyValue{attribute.String("payout_id", req.PayoutID.String())},
		func(ctx context.Context) (rail.Quote, error) { return i.Rail.Prepare(ctx, req) })
}

func (i instrumented) Submit(ctx context.Context, q rail.Quote, nonce, externalID string) (rail.Tx, error) {
	return observe(ctx, i.Rail, "submit", []attribute.KeyValue{attribute.String("payout_id", externalID)},
		func(ctx context.Context) (rail.Tx, error) { return i.Rail.Submit(ctx, q, nonce, externalID) })
}

func (i instrumented) Get(ctx context.Context, railTxID string) (rail.Tx, error) {
	return observe(ctx, i.Rail, "get", []attribute.KeyValue{attribute.String("rail_tx_id", railTxID)},
		func(ctx context.Context) (rail.Tx, error) { return i.Rail.Get(ctx, railTxID) })
}
