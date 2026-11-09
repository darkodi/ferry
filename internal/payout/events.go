package payout

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/payout/payoutdb"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/rail"
)

// SyncTopic is the outbox topic that asks the worker to re-fetch a
// payout's rail transaction (Service.Sync).
const SyncTopic = "rail.sync"

// EventResult says what RecordRailEvent did.
type EventResult struct {
	// Stored is false for a redelivery of an event already stored.
	Stored bool
	// PayoutID is the payout the event is about; uuid.Nil if the rail
	// transaction is not one of Ferry's.
	PayoutID uuid.UUID
	// Queued is true when a rail.sync was written for the payout.
	Queued bool
}

type syncEvent struct {
	PayoutID uuid.UUID `json:"payout_id"`
}

// RecordRailEvent stores a verified rail event and, when it is newer than
// every event stored for the same rail transaction, queues a rail.sync
// for its payout, all in one transaction. The caller answers the rail
// 2xx only after this returns.
//
// It never moves money. A webhook is only a hint: the worker's Sync
// fetches the transaction from the rail and applies what the rail says
// now. Storing first means a delivery is never lost; the version gate
// means a redelivered or out-of-date event causes no extra work.
func (s *Service) RecordRailEvent(ctx context.Context, ev rail.Event) (EventResult, error) {
	if ev.Rail == "" || ev.RailTxID == "" || ev.Version <= 0 {
		return EventResult{}, fmt.Errorf("%w: rail event needs a rail, a transaction id and a positive version", ErrInvalidRequest)
	}
	var res EventResult
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		res = EventResult{}
		q := payoutdb.New(tx)
		match := ev.RailTxID
		if ev.Reverses != "" {
			match = ev.Reverses // a return: the payout owns the original transaction
		}
		id, err := q.PayoutForRailTx(ctx, payoutdb.PayoutForRailTxParams{RailTxID: &match, Rail: ev.Rail})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("payout: match rail event: %w", err)
		default:
			res.PayoutID = id
		}
		newest, err := q.MaxRailEventVersion(ctx, payoutdb.MaxRailEventVersionParams{Rail: ev.Rail, RailTxID: ev.RailTxID})
		if err != nil {
			return fmt.Errorf("payout: latest rail event: %w", err)
		}
		var payoutID *uuid.UUID
		if res.PayoutID != uuid.Nil {
			payoutID = &res.PayoutID
		}
		var occurred = &ev.Occurred
		if ev.Occurred.IsZero() {
			occurred = nil
		}
		_, err = q.InsertRailEvent(ctx, payoutdb.InsertRailEventParams{
			Rail: ev.Rail, EventType: ev.Type, RailTxID: ev.RailTxID, EventVersion: ev.Version,
			Occurred: occurred, Payload: ev.Payload, PayoutID: payoutID,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil // redelivery
		case err != nil:
			return fmt.Errorf("payout: store rail event: %w", err)
		}
		res.Stored = true
		if res.PayoutID == uuid.Nil || ev.Version <= newest {
			return nil // not ours, or older than what we have seen
		}
		res.Queued = true
		return outbox.Write(ctx, tx, SyncTopic, res.PayoutID, syncEvent{PayoutID: res.PayoutID})
	})
	return res, err
}
