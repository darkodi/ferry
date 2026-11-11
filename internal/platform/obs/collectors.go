package obs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// CollectorErrors counts failed reads behind scraped gauges, so a broken
// gauge is visible rather than silently stale.
var CollectorErrors = counter("ferry_collector_errors_total",
	"Failed reads while collecting state gauges.", "collector")

var (
	descPayouts = prometheus.NewDesc("ferry_payouts",
		"Payouts by state.", []string{"status"}, nil)
	descPayoutOldest = prometheus.NewDesc("ferry_payout_oldest_in_state_seconds",
		"Age of the oldest payout in held or submitted, split by whether the rail reports a compliance review or RFI.",
		[]string{"status", "review"}, nil)
	descNeedsResolution = prometheus.NewDesc("ferry_payouts_needs_resolution",
		"Held payouts whose submit outcome is unknown, waiting for the resolver.", nil, nil)
	descOutboxUnpublished = prometheus.NewDesc("ferry_outbox_unpublished",
		"Outbox events not yet published.", nil, nil)
	descOutboxLag = prometheus.NewDesc("ferry_outbox_lag_seconds",
		"Age of the oldest unpublished outbox event (0 when none).", nil, nil)
	descBreaksOpen = prometheus.NewDesc("ferry_recon_breaks_open",
		"Open reconciliation breaks (not cleared, not resolved) by check.", []string{"check"}, nil)
	descBreakOldest = prometheus.NewDesc("ferry_recon_oldest_open_break_seconds",
		"Age of the oldest open reconciliation break, by check.", []string{"check"}, nil)
	descReconAge = prometheus.NewDesc("ferry_recon_last_run_age_seconds",
		"Time since the last reconciliation run finished (absent before the first).", nil, nil)
)

// DBCollector reads Ferry's state from Postgres on each scrape. Register
// it in one process only (the API), so the series are not duplicated.
type DBCollector struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewDBCollector returns a collector over pool.
func NewDBCollector(pool *pgxpool.Pool) *DBCollector {
	return &DBCollector{pool: pool, timeout: 3 * time.Second}
}

// Describe implements prometheus.Collector.
func (c *DBCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descPayouts, descPayoutOldest, descNeedsResolution, descOutboxUnpublished,
		descOutboxLag, descBreaksOpen, descBreakOldest, descReconAge} {
		ch <- d
	}
}

// Collect implements prometheus.Collector. Each read is independent: one
// that fails is counted in ferry_collector_errors_total and skipped.
func (c *DBCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	for name, read := range map[string]func(context.Context, chan<- prometheus.Metric) error{
		"payouts": c.payouts, "outbox": c.outbox, "recon": c.recon,
	} {
		if err := read(ctx, ch); err != nil {
			CollectorErrors.WithLabelValues(name).Inc()
		}
	}
}

// payoutStates are reported even at zero, so alerts and panels see 0
// rather than no data.
var payoutStates = []string{"created", "quoted", "held", "submitted", "settled", "reversed", "failed", "expired"}

func (c *DBCollector) payouts(ctx context.Context, ch chan<- prometheus.Metric) error {
	counts := make(map[string]float64, len(payoutStates))
	rows, err := c.pool.Query(ctx, `SELECT status, count(*) FROM payouts GROUP BY status`)
	if err != nil {
		return fmt.Errorf("obs: payouts by state: %w", err)
	}
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return fmt.Errorf("obs: payouts by state: %w", err)
		}
		counts[s] = float64(n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("obs: payouts by state: %w", err)
	}
	for _, s := range payoutStates {
		ch <- prometheus.MustNewConstMetric(descPayouts, prometheus.GaugeValue, counts[s], s)
	}

	// Time in state: payouts.updated_at moves only on transitions; the
	// rail's sub-status (review, RFI) is on the latest rail attempt.
	oldest := map[[2]string]float64{}
	rows, err = c.pool.Query(ctx, `
		SELECT p.status,
		       coalesce(r.sub_status ~ '(UnderReview|RFI|review)', false) AS review,
		       max(extract(epoch FROM now() - p.updated_at))::float8
		FROM payouts p
		LEFT JOIN LATERAL (
			SELECT sub_status FROM rail_transactions WHERE payout_id = p.id ORDER BY attempt DESC LIMIT 1
		) r ON true
		WHERE p.status IN ('held', 'submitted')
		GROUP BY 1, 2`)
	if err != nil {
		return fmt.Errorf("obs: payout ages: %w", err)
	}
	for rows.Next() {
		var s string
		var review bool
		var age float64
		if err := rows.Scan(&s, &review, &age); err != nil {
			rows.Close()
			return fmt.Errorf("obs: payout ages: %w", err)
		}
		oldest[[2]string{s, strconv.FormatBool(review)}] = age
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("obs: payout ages: %w", err)
	}
	for _, s := range []string{"held", "submitted"} {
		for _, rv := range []string{"false", "true"} {
			ch <- prometheus.MustNewConstMetric(descPayoutOldest, prometheus.GaugeValue, oldest[[2]string{s, rv}], s, rv)
		}
	}

	var stuck int64
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE status = 'held' AND needs_resolution`).Scan(&stuck); err != nil {
		return fmt.Errorf("obs: needs resolution: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(descNeedsResolution, prometheus.GaugeValue, float64(stuck))
	return nil
}

func (c *DBCollector) outbox(ctx context.Context, ch chan<- prometheus.Metric) error {
	var n int64
	var lag float64
	err := c.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
		FROM outbox WHERE published_at IS NULL`).Scan(&n, &lag)
	if err != nil {
		return fmt.Errorf("obs: outbox: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(descOutboxUnpublished, prometheus.GaugeValue, float64(n))
	ch <- prometheus.MustNewConstMetric(descOutboxLag, prometheus.GaugeValue, lag)
	return nil
}

func (c *DBCollector) recon(ctx context.Context, ch chan<- prometheus.Metric) error {
	open := map[string][2]float64{"internal": {}, "chain": {}, "rail": {}}
	rows, err := c.pool.Query(ctx, `
		SELECT check_name, count(*), coalesce(extract(epoch FROM now() - min(first_seen)), 0)::float8
		FROM recon_breaks WHERE cleared_at IS NULL AND resolved_at IS NULL
		GROUP BY check_name`)
	if err != nil {
		return fmt.Errorf("obs: recon breaks: %w", err)
	}
	for rows.Next() {
		var check string
		var n int64
		var age float64
		if err := rows.Scan(&check, &n, &age); err != nil {
			rows.Close()
			return fmt.Errorf("obs: recon breaks: %w", err)
		}
		open[check] = [2]float64{float64(n), age}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("obs: recon breaks: %w", err)
	}
	for check, v := range open {
		ch <- prometheus.MustNewConstMetric(descBreaksOpen, prometheus.GaugeValue, v[0], check)
		ch <- prometheus.MustNewConstMetric(descBreakOldest, prometheus.GaugeValue, v[1], check)
	}

	var age *float64
	if err := c.pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - max(finished_at))::float8 FROM recon_runs WHERE finished_at IS NOT NULL`).Scan(&age); err != nil {
		return fmt.Errorf("obs: recon runs: %w", err)
	}
	if age != nil {
		ch <- prometheus.MustNewConstMetric(descReconAge, prometheus.GaugeValue, *age)
	}
	return nil
}

// QueueDepth is a source of a queue's approximate message count.
type QueueDepth func(ctx context.Context) (int64, error)

var descQueue = prometheus.NewDesc("ferry_queue_messages",
	"Approximate messages waiting in a queue (the payout queue and its dead-letter queue).", []string{"queue"}, nil)

// QueueCollector reports queue depths on each scrape (the worker
// registers it).
type QueueCollector struct {
	queues map[string]QueueDepth
}

// NewQueueCollector returns a collector over named queues.
func NewQueueCollector(queues map[string]QueueDepth) *QueueCollector {
	return &QueueCollector{queues: queues}
}

// Describe implements prometheus.Collector.
func (*QueueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- descQueue }

// Collect implements prometheus.Collector.
func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for name, depth := range c.queues {
		n, err := depth(ctx)
		if err != nil {
			CollectorErrors.WithLabelValues("queue").Inc()
			continue
		}
		ch <- prometheus.MustNewConstMetric(descQueue, prometheus.GaugeValue, float64(n), name)
	}
}
