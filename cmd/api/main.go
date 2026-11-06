// Command api serves Ferry's HTTP API.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL         Postgres connection string (required)
//	FERRY_ADMIN_KEY      operator key for admin routes, at least 32 characters (required)
//	FERRY_ENV            "dev" enables /v1/dev routes, an auto-settling mock
//	                     rail, and runs the relay, worker and resolver in-process
//	FERRY_ADDR           listen address (default 127.0.0.1:8080)
//	FERRY_SQS_QUEUE_URL  dev mode: payout queue (ElasticMQ); empty uses an in-memory queue
//	FERRY_SQS_ENDPOINT   SQS endpoint override, e.g. http://localhost:9324
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/api"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail/mock"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("api stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	adminKey := os.Getenv("FERRY_ADMIN_KEY")
	dev := os.Getenv("FERRY_ENV") == "dev"
	addr := os.Getenv("FERRY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	if len(adminKey) < 32 {
		return errors.New("FERRY_ADMIN_KEY must be at least 32 characters")
	}

	// Cancelled on Ctrl-C or SIGTERM, which starts a graceful shutdown.
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

	// M3 has only the mock rail; the Noah adapter replaces it in M5.
	rl := mock.New(time.Now)
	rl.AutoSettle = dev
	payouts := payout.NewService(pool, rl, house, time.Now)

	// The mock rail lives in this process's memory, so the worker that
	// submits to it must run here too. With a shared rail (M5) the relay
	// and worker run as their own binaries (cmd/relay, cmd/worker).
	var background sync.WaitGroup
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer func() { stopBackground(); background.Wait() }()
	if dev {
		if err := startBackground(bgCtx, &background, pool, payouts, log); err != nil {
			return err
		}
	} else {
		log.Warn("payouts will stay held: no worker runs outside dev mode until the Noah rail (M5)")
	}

	handler := api.New(api.Config{
		Pool:     pool,
		Payouts:  payouts,
		Idem:     idempotency.NewStore(pool, time.Now, 30*time.Second, log),
		House:    house,
		AdminKey: adminKey,
		Dev:      dev,
		Log:      log,
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, // slow-header (Slowloris) protection
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "dev", dev)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Stop taking requests first, then the background loops (deferred).
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// startBackground runs the outbox relay, payout worker and resolver until
// ctx is cancelled. They use ElasticMQ when FERRY_SQS_QUEUE_URL is set, so
// dev exercises the real queue path; otherwise an in-memory queue.
func startBackground(ctx context.Context, wg *sync.WaitGroup, pool *pgxpool.Pool, payouts *payout.Service, log *slog.Logger) error {
	var pub queue.Publisher
	var con queue.Consumer
	if url := os.Getenv("FERRY_SQS_QUEUE_URL"); url != "" {
		q, err := queue.NewSQS(ctx, os.Getenv("FERRY_SQS_ENDPOINT"), url)
		if err != nil {
			return err
		}
		q.Wait = 1 // short long-poll, so shutdown is quick
		pub, con = q, q
	} else {
		q := queue.NewMemory()
		pub, con = q, q
	}
	resolver := payout.NewResolver(payouts, log)
	resolver.StaleAfter = 10 * time.Second // dev: resolve quickly
	wg.Go(func() { outbox.NewRelay(pool, pub, time.Now, log).Run(ctx, 200*time.Millisecond) })
	wg.Go(func() { payout.NewWorker(payouts, con, log).Run(ctx) })
	wg.Go(func() { resolver.Run(ctx, 5*time.Second) })
	log.Info("background started", "relay", true, "worker", true, "resolver", true, "sqs", os.Getenv("FERRY_SQS_QUEUE_URL") != "")
	return nil
}
