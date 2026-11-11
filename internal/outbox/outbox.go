// Package outbox writes events in the same transaction as the state change
// they describe, so an event exists if and only if the change committed.
// Relay then publishes them to a queue.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/platform/obs"
)

const insert = `INSERT INTO outbox (topic, aggregate_id, payload, traceparent) VALUES ($1, $2, $3, $4)`

// Write adds an event to the outbox inside tx. payload is stored as JSON.
// The trace in ctx, if any, is stored with it, so the worker that handles
// the event continues the same trace.
func Write(ctx context.Context, tx pgx.Tx, topic string, aggregateID uuid.UUID, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s: %w", topic, err)
	}
	var tp *string
	if s := obs.TraceParent(ctx); s != "" {
		tp = &s
	}
	if _, err := tx.Exec(ctx, insert, topic, aggregateID, b, tp); err != nil {
		return fmt.Errorf("outbox: write %s: %w", topic, err)
	}
	return nil
}
