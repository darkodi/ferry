// Package inbox records which messages each consumer has handled, so a
// redelivered message is acknowledged without repeating its effect.
//
// For an effect that is purely a database change, Handle records the
// message in the same transaction as the effect: either both commit or
// neither does. An effect that calls an external system between
// transactions (submitting to the rail) cannot share a transaction with
// the inbox row; there the effect itself must be idempotent, and Seen and
// Record only let a duplicate skip the work early.
package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/platform/db"
)

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Seen reports whether consumer has already handled messageID.
func Seen(ctx context.Context, q DBTX, consumer, messageID string) (bool, error) {
	var one int
	err := q.QueryRow(ctx,
		`SELECT 1 FROM inbox WHERE consumer = $1 AND message_id = $2`, consumer, messageID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inbox: lookup: %w", err)
	}
	return true, nil
}

// Record marks messageID as handled by consumer. It reports false if it
// was already recorded.
func Record(ctx context.Context, q DBTX, consumer, messageID string) (bool, error) {
	tag, err := q.Exec(ctx,
		`INSERT INTO inbox (consumer, message_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		consumer, messageID)
	if err != nil {
		return false, fmt.Errorf("inbox: record: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Handle runs fn and records messageID in one transaction. If the message
// was already recorded, fn is not run and Handle reports false.
// fn must only use tx: it may run more than once (db.WithTx retries).
func Handle(ctx context.Context, pool *pgxpool.Pool, consumer, messageID string, fn func(pgx.Tx) error) (bool, error) {
	var fresh bool
	err := db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Record first: a concurrent duplicate blocks on the row's key
		// until this tx ends, then finds it and skips.
		var err error
		if fresh, err = Record(ctx, tx, consumer, messageID); err != nil || !fresh {
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return false, err
	}
	return fresh, nil
}
