// Command relay publishes outbox rows to the payout queue.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL         Postgres connection string (required)
//	FERRY_SQS_QUEUE_URL  queue to publish to (required)
//	FERRY_SQS_ENDPOINT   SQS endpoint override, e.g. ElasticMQ locally
//	AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY  standard AWS settings
//
// Several relays may run at once: each claims different rows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/platform/obs"
	"github.com/darkodi/ferry/internal/platform/queue"
)

func main() {
	log := obs.NewLogger()
	if err := run(log); err != nil {
		log.Error("relay stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	// Cancelled on Ctrl-C or SIGTERM: the current round finishes, then Run returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTraces, err := obs.Setup(ctx, "ferry-relay")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTraces(context.Background()) }()
	if err := obs.Serve(ctx, envOr("FERRY_METRICS_ADDR", "127.0.0.1:9102"), log); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	q, err := queue.NewSQS(ctx, os.Getenv("FERRY_SQS_ENDPOINT"), os.Getenv("FERRY_SQS_QUEUE_URL"))
	if err != nil {
		return err
	}

	log.Info("relay started")
	outbox.NewRelay(pool, q, time.Now, log).Run(ctx, 500*time.Millisecond)
	log.Info("relay stopped")
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
