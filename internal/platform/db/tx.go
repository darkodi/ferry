// Package db holds Postgres helpers shared by Ferry's binaries.
package db

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxAttempts is how many times WithTx runs fn before giving up on
// serialization failures and deadlocks.
const MaxAttempts = 5

// WithTx runs fn in a transaction and commits it. If Postgres aborts the
// transaction with a serialization failure (40001) or a deadlock (40P01),
// the whole transaction is retried with a short random backoff, up to
// MaxAttempts times. Any other error rolls back and is returned as is.
//
// Because fn may run more than once, it must only touch the database
// through tx: no rail calls, no queue publishes, no other side effects.
func WithTx(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		err = runOnce(ctx, pool, opts, fn)
		if err == nil || !retryable(err) || attempt == MaxAttempts {
			return err
		}
		// Exponential backoff with full jitter: 0..(5ms * 2^attempt).
		backoff := time.Duration(rand.Int64N(int64(5*time.Millisecond) << attempt)) //nolint:gosec // jitter only; not security-sensitive
		select {
		case <-ctx.Done():
			return fmt.Errorf("db: retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
	return err
}

func runOnce(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so deferring it is
	// safe and covers every early return and panic.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
