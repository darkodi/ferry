// Command worker consumes payout events (submitting held payouts to the
// rail) and runs the resolver for payouts that stopped moving.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL         Postgres connection string (required)
//	FERRY_SQS_QUEUE_URL  queue to read from (required)
//	FERRY_SQS_ENDPOINT   SQS endpoint override, e.g. ElasticMQ locally
//	FERRY_RAIL           which rail to submit to: "noah" (see internal/rail/noah.FromEnv for NOAH_*)
//	FERRY_RESOLVER_STALE how long a payout may sit held or submitted before the
//	                     resolver re-drives it (default 1m); without a public webhook
//	                     URL this is how often Noah is polled
//
// The mock rail lives in one process's memory and cannot be shared with
// the API process that made the quotes, so it is refused here; in dev,
// cmd/api with FERRY_ENV=dev runs the same worker in-process instead.
//
// The worker never reads bank details, so it does not get FERRY_DATA_KEY.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/noah"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	rl, err := railFromEnv(os.Getenv("FERRY_RAIL"))
	if err != nil {
		return err
	}

	// Cancelled on Ctrl-C or SIGTERM: the current batch finishes first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	house, err := ledger.OpenHouseAccounts(ctx, pool)
	if err != nil {
		return fmt.Errorf("open house accounts: %w", err)
	}
	q, err := queue.NewSQS(ctx, os.Getenv("FERRY_SQS_ENDPOINT"), os.Getenv("FERRY_SQS_QUEUE_URL"))
	if err != nil {
		return err
	}

	dests, err := destination.NewStore(pool, rl.Name(), nil)
	if err != nil {
		return err
	}
	svc := payout.NewService(pool, rl, dests, house, time.Now)
	resolver := payout.NewResolver(svc, log)
	if v := os.Getenv("FERRY_RESOLVER_STALE"); v != "" {
		if resolver.StaleAfter, err = time.ParseDuration(v); err != nil || resolver.StaleAfter <= 0 {
			return fmt.Errorf("FERRY_RESOLVER_STALE %q: want a positive duration such as 1m", v)
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { payout.NewWorker(svc, q, log).Run(ctx) })
	// Look as often as the staleness threshold, at most every 15 seconds.
	every := min(15*time.Second, resolver.StaleAfter)
	wg.Go(func() { resolver.Run(ctx, every) })
	log.Info("worker started", "rail", rl.Name(), "resolver_stale", resolver.StaleAfter.String())
	wg.Wait()
	log.Info("worker stopped")
	return nil
}

// railFromEnv picks the rail adapter. Only a rail reachable from several
// processes can serve a separate worker.
func railFromEnv(name string) (rail.Rail, error) {
	switch name {
	case "noah":
		return noah.FromEnv(os.Getenv, time.Now)
	case "", "mock":
		return nil, errors.New("the mock rail lives in one process's memory and cannot serve a separate worker; " +
			"set FERRY_RAIL=noah, or run cmd/api with FERRY_ENV=dev, which runs the worker in-process")
	default:
		return nil, fmt.Errorf("unknown FERRY_RAIL %q", name)
	}
}
