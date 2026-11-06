// Command worker consumes payout events (submitting held payouts to the
// rail) and runs the resolver for payouts that stopped moving.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL         Postgres connection string (required)
//	FERRY_SQS_QUEUE_URL  queue to read from (required)
//	FERRY_SQS_ENDPOINT   SQS endpoint override, e.g. ElasticMQ locally
//	FERRY_RAIL           which rail to submit to; "noah" arrives in M5
//
// Until M5 the only rail is the in-memory mock, which cannot be shared
// with the API process that made the quotes. The worker therefore refuses
// to start; in dev, cmd/api with FERRY_ENV=dev runs the same worker
// in-process instead.
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

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail"
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

	svc := payout.NewService(pool, rl, house, time.Now)
	var wg sync.WaitGroup
	wg.Go(func() { payout.NewWorker(svc, q, log).Run(ctx) })
	wg.Go(func() { payout.NewResolver(svc, log).Run(ctx, 15*time.Second) })
	log.Info("worker started", "rail", rl.Name())
	wg.Wait()
	log.Info("worker stopped")
	return nil
}

// railFromEnv picks the rail adapter. The Noah adapter is added in M5;
// until then every choice is an error.
//
//nolint:unparam // the Rail result is always nil until M5 adds a shareable adapter
func railFromEnv(name string) (rail.Rail, error) {
	switch name {
	case "", "mock":
		return nil, errors.New("the mock rail lives in one process's memory and cannot serve a separate worker; " +
			"run cmd/api with FERRY_ENV=dev, which runs the worker in-process, until the Noah rail (M5)")
	default:
		return nil, fmt.Errorf("unknown FERRY_RAIL %q", name)
	}
}
