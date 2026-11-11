// Package obs is Ferry's observability plumbing: Prometheus metrics,
// OpenTelemetry traces, and a log handler that stamps every line with
// the trace it belongs to.
//
// Metrics are defined here, in one place, so their names, labels and help
// texts stay consistent and the alert rules and dashboard in deploy/ can
// be checked against them (TestDeployReferencesKnownMetrics). Each binary
// serves them on its own listener (Serve), separate from the public API.
package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every Ferry metric. A dedicated registry (not the
// global default) keeps library metrics out unless added on purpose.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
}

// Event metrics, updated where things happen. State metrics (payouts by
// state, outbox lag, open breaks) are read from the database on scrape
// instead: see DBCollector.
var (
	HTTPRequests = counter("ferry_http_requests_total",
		"API requests by method, route pattern and status code.", "method", "route", "code")
	HTTPDuration = histogram("ferry_http_request_duration_seconds",
		"API request latency by method and route pattern.", prometheus.DefBuckets, "method", "route")

	PayoutTransitions = counter("ferry_payout_transitions_total",
		"Payout state transitions.", "from", "to")
	PayoutTransitionAge = histogram("ferry_payout_transition_age_seconds",
		"Time from a payout's creation to reaching a state.",
		[]float64{0.1, 0.5, 1, 5, 15, 60, 300, 1800, 3600, 6 * 3600, 24 * 3600, 3 * 24 * 3600}, "to")
	RailMismatches = counter("ferry_rail_mismatch_total",
		"Rail reports that cannot be true for the payout (applied nothing; needs a human).", "rail")

	RailRequests = counter("ferry_rail_requests_total",
		"Rail calls by operation and outcome (ok, rejected, not_found, error).", "rail", "op", "outcome")
	RailDuration = histogram("ferry_rail_request_duration_seconds",
		"Rail call latency by operation.", prometheus.DefBuckets, "rail", "op")

	Webhooks = counter("ferry_webhooks_total",
		"Rail webhooks by result (queued, stored, duplicate, ignored, bad_signature, invalid, error).", "rail", "result")

	WorkerMessages = counter("ferry_worker_messages_total",
		"Queue messages handled by the payout worker, by topic and result (ok, duplicate, error).", "topic", "result")

	WatcherLag = gauge("ferry_watcher_lag_blocks",
		"Blocks (slots) between the chain head the watcher reads and its scan cursor.", "chain")
	WatcherRounds = counter("ferry_watcher_rounds_total",
		"Watcher rounds by result (ok, error).", "chain", "result")
	WatcherDeposits = counter("ferry_watcher_deposits_total",
		"Deposits by event (found, confirmed, orphaned).", "chain", "event")
	WatcherDeepReorgs = counter("ferry_watcher_deep_reorg_total",
		"Reorgs deeper than the stored block history (the watcher stops).", "chain")
	WatcherCreditedReorged = counter("ferry_watcher_credited_reorged_total",
		"Credited deposits whose block a reorg removed (a human must post a correction).", "chain")
)

// OutboxPublished counts outbox events published to the queue.
var OutboxPublished = func() prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "ferry_outbox_published_total", Help: "Outbox events published to the queue."})
	Registry.MustRegister(c)
	return c
}()

// Serve serves /metrics on addr until ctx is done. An empty addr, or
// "off", serves nothing. Keep addr on loopback or a private network: metrics reveal
// volumes and balances' shape, not secrets, but they are not public.
func Serve(ctx context.Context, addr string, log *slog.Logger) error {
	if addr == "" || addr == "off" {
		return nil
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("obs: metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError)}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		// ctx is done here; keep its values but not its cancellation.
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("metrics listening", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server", "error", err)
		}
	}()
	return nil
}

// Names lists every Ferry metric name: the event metrics and the scraped
// state gauges. Tests check the alert rules and dashboard against it.
func Names() []string {
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		for _, c := range []prometheus.Collector{
			HTTPRequests, HTTPDuration, PayoutTransitions, PayoutTransitionAge, RailMismatches, RailRequests,
			RailDuration, Webhooks, WorkerMessages, OutboxPublished, WatcherLag, WatcherRounds, WatcherDeposits,
			WatcherDeepReorgs, WatcherCreditedReorged, CollectorErrors, &DBCollector{}, &QueueCollector{},
		} {
			c.Describe(ch)
		}
		close(ch)
	}()
	var out []string
	for d := range ch {
		out = append(out, fqName(d))
	}
	return out
}

// fqName extracts a Desc's metric name; client_golang does not export it.
func fqName(d *prometheus.Desc) string {
	s := d.String() // Desc{fqName: "ferry_x", help: ...}
	const key = `fqName: "`
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	s = s[i+len(key):]
	return s[:strings.IndexByte(s, '"')]
}
