package payout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/inbox"
	"github.com/darkodi/ferry/internal/platform/queue"
)

// SubmitConsumer is the worker's name in the inbox table.
const SubmitConsumer = "payout-submit"

// Worker consumes payout events and submits held payouts to the rail.
//
// A delivery is acknowledged only after its work is done. If the worker
// fails or dies first, the queue delivers it again, and after
// queue.MaxReceives attempts moves it to the dead-letter queue.
//
// Duplicates are harmless at three levels: the inbox skips a message
// already handled; Submit does nothing for a payout that is no longer
// held; and a resubmit reuses the stored nonce, so the rail cannot create
// a second transaction. The inbox row cannot share a transaction with
// Submit, because the rail call sits between Submit's transactions; it is
// the cheap first check, not the guarantee.
type Worker struct {
	svc  *Service
	q    queue.Consumer
	log  *slog.Logger
	Wait time.Duration // pause after an empty receive (queues that do not long-poll)
}

// NewWorker returns a worker reading from q.
func NewWorker(svc *Service, q queue.Consumer, log *slog.Logger) *Worker {
	return &Worker{svc: svc, q: q, log: log, Wait: 200 * time.Millisecond}
}

// Run handles deliveries until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.ErrorContext(ctx, "payout worker", "error", err)
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.Wait):
		}
	}
}

// RunOnce receives one batch and handles each delivery. It returns how
// many deliveries were received. A delivery that fails is logged and left
// unacknowledged for redelivery; the rest of the batch still runs.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	ds, err := w.q.Receive(ctx, 10)
	if err != nil {
		return 0, fmt.Errorf("payout worker: receive: %w", err)
	}
	for _, d := range ds {
		if err := w.Handle(ctx, d); err != nil {
			w.log.ErrorContext(ctx, "payout worker: delivery failed; it will be redelivered",
				"message_id", d.ID, "topic", d.Topic, "receives", d.Receives, "error", err)
			continue
		}
		if err := w.q.Ack(ctx, d); err != nil {
			// Not fatal: the message comes back and is skipped as a duplicate.
			w.log.ErrorContext(ctx, "payout worker: ack", "message_id", d.ID, "error", err)
		}
	}
	return len(ds), nil
}

// Handle does the work for one delivery. A nil error means it may be
// acknowledged.
func (w *Worker) Handle(ctx context.Context, d queue.Delivery) error {
	if d.Topic != "payout."+string(Held) {
		return nil // no work for other events yet
	}
	if d.ID == "" {
		return errors.New("message has no id")
	}
	var ev event
	if err := json.Unmarshal(d.Body, &ev); err != nil {
		return fmt.Errorf("decode %s: %w", d.Topic, err)
	}
	if ev.PayoutID == uuid.Nil {
		return fmt.Errorf("decode %s: no payout_id", d.Topic)
	}

	seen, err := inbox.Seen(ctx, w.svc.pool, SubmitConsumer, d.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}
	p, err := w.svc.Submit(ctx, ev.PayoutID)
	if err != nil {
		return fmt.Errorf("submit payout %s: %w", ev.PayoutID, err)
	}
	// An unknown outcome is not an error here: the payout keeps
	// NeedsResolution and the resolver retries it with the same nonce.
	w.log.InfoContext(ctx, "payout submitted", "payout_id", p.ID, "status", p.Status,
		"needs_resolution", p.NeedsResolution, "message_id", d.ID)
	if _, err := inbox.Record(ctx, w.svc.pool, SubmitConsumer, d.ID); err != nil {
		return err
	}
	return nil
}
