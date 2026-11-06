package outbox

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/outbox/outboxdb"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/platform/queue"
)

// Relay publishes outbox rows to a queue.
//
// Each round has three steps, and only the middle one talks to the queue:
//
//  1. claim a batch of unpublished rows with a short lease (one short tx);
//  2. publish them, outside any transaction;
//  3. mark them published (another short tx).
//
// A relay that dies between 2 and 3 leaves the lease to run out and the
// next round publishes those rows again. Delivery is therefore at least
// once, and consumers dedup on the message id (the outbox row id).
type Relay struct {
	pool  *pgxpool.Pool
	pub   queue.Publisher
	now   func() time.Time
	log   *slog.Logger
	Lease time.Duration // how long a claimed row is reserved for one relay
	Batch int32         // rows per round
}

// NewRelay returns a relay with a 30-second lease and batches of 100.
func NewRelay(pool *pgxpool.Pool, pub queue.Publisher, now func() time.Time, log *slog.Logger) *Relay {
	return &Relay{pool: pool, pub: pub, now: now, log: log, Lease: 30 * time.Second, Batch: 100}
}

// Run publishes rounds every interval until ctx is cancelled. A full
// batch is followed immediately by another round, so a backlog drains
// without waiting.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	for {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.ErrorContext(ctx, "outbox relay", "error", err)
		}
		if err == nil && n == int(r.Batch) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// RunOnce claims, publishes and marks one batch. It returns how many rows
// were published. Rows published before an error are still marked, so a
// failing queue does not cause the whole batch to be sent again.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	rows, err := r.claim(ctx)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	var published []int64
	var pubErr error
	for _, row := range rows {
		err := r.pub.Publish(ctx, queue.Message{
			ID:    strconv.FormatInt(row.ID, 10),
			Topic: row.Topic,
			Body:  row.Payload,
		})
		if err != nil {
			pubErr = fmt.Errorf("outbox: publish %d: %w", row.ID, err)
			break // keep order: later rows wait for the next round
		}
		published = append(published, row.ID)
	}
	if err := r.mark(ctx, published); err != nil {
		return 0, err
	}
	return len(published), pubErr
}

// claim leases the next batch of unpublished rows, oldest first.
func (r *Relay) claim(ctx context.Context) ([]outboxdb.ClaimRow, error) {
	var rows []outboxdb.ClaimRow
	now := r.now()
	err := db.WithTx(ctx, r.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		rows, err = outboxdb.New(tx).Claim(ctx, outboxdb.ClaimParams{
			ClaimedUntil: now.Add(r.Lease), Now: now, MaxRows: r.Batch,
		})
		if err != nil {
			return fmt.Errorf("outbox: claim: %w", err)
		}
		return nil
	})
	// UPDATE … RETURNING does not keep the subquery's order.
	slices.SortFunc(rows, func(a, b outboxdb.ClaimRow) int { return cmp.Compare(a.ID, b.ID) })
	return rows, err
}

func (r *Relay) mark(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	err := outboxdb.New(r.pool).MarkPublished(ctx, outboxdb.MarkPublishedParams{PublishedAt: r.now(), Ids: ids})
	if err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}
