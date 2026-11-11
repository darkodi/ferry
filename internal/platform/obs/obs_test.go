package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/darkodi/ferry/internal/platform/obs"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/mock"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool     *pgxpool.Pool
	recorder = tracetest.NewSpanRecorder()
)

func TestMain(m *testing.M) {
	flag.Parse()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer tdb.Close()
	if pool, err = pgxpool.New(ctx, tdb.DSN); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	return m.Run()
}

// Every ferry_* metric the alert rules and the dashboard use exists in
// code, so renaming a metric cannot silently break an alert.
func TestDeployReferencesKnownMetrics(t *testing.T) {
	known := map[string]bool{}
	for _, n := range obs.Names() {
		known[n] = true
	}
	if len(known) < 20 {
		t.Fatalf("only %d metric names known: %v", len(known), obs.Names())
	}
	nameRE := regexp.MustCompile(`ferry_[a-z_]+`)
	for _, path := range []string{"../../../deploy/prometheus/alerts.yml", "../../../deploy/grafana/dashboards/ferry.json"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		used := 0
		for _, n := range nameRE.FindAllString(string(b), -1) {
			// Histograms are queried through their _bucket, _sum and _count series.
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(n, "_bucket"), "_sum"), "_count")
			if !known[base] {
				t.Errorf("%s uses %s, which no Ferry metric defines", path, n)
			}
			used++
		}
		if used == 0 {
			t.Errorf("%s references no Ferry metrics", path)
		}
	}
	var dash map[string]any
	b, _ := os.ReadFile("../../../deploy/grafana/dashboards/ferry.json")
	if err := json.Unmarshal(b, &dash); err != nil || dash["uid"] != "ferry-overview" {
		t.Fatalf("dashboard JSON: %v", err)
	}
}

// Log lines written with a traced context carry its trace and span ids.
func TestLogHandlerAddsTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(obs.NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	ctx, span := obs.Tracer.Start(context.Background(), "test")
	log.InfoContext(ctx, "hello")
	span.End()
	log.Info("untraced")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first, second map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first["trace_id"] != span.SpanContext().TraceID().String() || first["span_id"] == nil {
		t.Fatalf("traced line %v", first)
	}
	if _, ok := second["trace_id"]; ok {
		t.Fatalf("untraced line has a trace_id: %v", second)
	}
}

// A traceparent written by one process continues the same trace in
// another; garbage is ignored.
func TestTraceParentRoundTrip(t *testing.T) {
	ctx, span := obs.Tracer.Start(context.Background(), "producer")
	defer span.End()
	tp := obs.TraceParent(ctx)
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`).MatchString(tp) {
		t.Fatalf("traceparent %q", tp)
	}
	_, child := obs.Tracer.Start(obs.WithTraceParent(context.Background(), tp), "consumer")
	defer child.End()
	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Fatal("the consumer did not continue the producer's trace")
	}
	if got := trace.SpanContextFromContext(obs.WithTraceParent(context.Background(), "garbage")); got.IsValid() {
		t.Fatal("a malformed traceparent produced a span context")
	}
	if obs.TraceParent(context.Background()) != "" {
		t.Fatal("no span, yet a traceparent")
	}
}

// The rail decorator records each call's outcome and a span, and passes
// the rail's own errors through unchanged.
func TestInstrumentRail(t *testing.T) {
	r := obs.InstrumentRail(mock.New(time.Now))
	before := testutil.ToFloat64(obs.RailRequests.WithLabelValues("mock", "prepare", "rejected"))
	_, err := r.Prepare(context.Background(), rail.QuoteRequest{FiatAmount: 100, FiatCurrency: "JPY"})
	if !errors.Is(err, rail.ErrRejected) {
		t.Fatalf("got %v, want the rail's ErrRejected", err)
	}
	if got := testutil.ToFloat64(obs.RailRequests.WithLabelValues("mock", "prepare", "rejected")); got != before+1 {
		t.Fatalf("rejected counter %v, want %v", got, before+1)
	}
	if _, err := r.Prepare(context.Background(), rail.QuoteRequest{FiatAmount: 100, FiatCurrency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(obs.RailRequests.WithLabelValues("mock", "prepare", "ok")) < 1 {
		t.Fatal("ok outcome not counted")
	}
	found := slices.ContainsFunc(recorder.Ended(), func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "rail.prepare" })
	if !found {
		t.Fatal("no rail.prepare span")
	}
}

// The state gauges read the database; every payout state is reported,
// zero included, so panels and alerts never see "no data".
func TestDBCollector(t *testing.T) {
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(obs.NewDBCollector(pool))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := map[string]int{}
	for _, mf := range mfs {
		series[mf.GetName()] = len(mf.GetMetric())
	}
	for name, want := range map[string]int{
		"ferry_payouts": 8, "ferry_payout_oldest_in_state_seconds": 4, "ferry_payouts_needs_resolution": 1,
		"ferry_outbox_unpublished": 1, "ferry_outbox_lag_seconds": 1, "ferry_recon_breaks_open": 3,
		"ferry_recon_oldest_open_break_seconds": 3,
	} {
		if series[name] != want {
			t.Errorf("%s: %d series, want %d", name, series[name], want)
		}
	}
	if _, ok := series["ferry_recon_last_run_age_seconds"]; ok {
		t.Error("recon age reported before any run")
	}
	if n := testutil.ToFloat64(obs.CollectorErrors.WithLabelValues("payouts")); n != 0 {
		t.Errorf("collector errors: %v", n)
	}
}
