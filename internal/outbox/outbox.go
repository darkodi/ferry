// Package outbox writes events in the same transaction as the state change
// they describe, so an event exists if and only if the change committed.
// The relay that publishes them to a queue arrives in M3.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const insert = `INSERT INTO outbox (topic, aggregate_id, payload) VALUES ($1, $2, $3)`

// Write adds an event to the outbox inside tx. payload is stored as JSON.
func Write(ctx context.Context, tx pgx.Tx, topic string, aggregateID uuid.UUID, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s: %w", topic, err)
	}
	if _, err := tx.Exec(ctx, insert, topic, aggregateID, b); err != nil {
		return fmt.Errorf("outbox: write %s: %w", topic, err)
	}
	return nil
}
