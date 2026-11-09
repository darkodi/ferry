// Command api serves Ferry's HTTP API.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL         Postgres connection string (required)
//	FERRY_ADMIN_KEY      operator key for admin routes, at least 32 characters (required)
//	FERRY_ENV            "dev" enables /v1/dev routes and runs the relay, worker
//	                     and resolver in-process (with the mock rail, it also settles at once)
//	FERRY_ADDR           listen address (default 127.0.0.1:8080)
//	FERRY_RAIL           "mock" (default) or "noah"; see internal/rail/noah.FromEnv for NOAH_*.
//	                     With noah, POST /v1/webhooks/noah verifies Noah's webhooks with Noah's
//	                     published key, or NOAH_WEBHOOK_PUBLIC_KEY (a PEM file) if set
//	FERRY_DATA_KEY       base64 32-byte key that encrypts saved bank details (required)
//	FERRY_SQS_QUEUE_URL  dev mode: payout queue (ElasticMQ); empty uses an in-memory queue
//	FERRY_SQS_ENDPOINT   SQS endpoint override, e.g. http://localhost:9324
//	FERRY_EVM_CHAIN      EVM chain name in the database (default evm-local)
//	FERRY_EVM_XPUB       account xpub for customer deposit addresses; empty assigns none
//	FERRY_SOLANA_CHAIN   Solana chain name in the database (default solana-local)
//	FERRY_SOLANA_AUTHORITY  authority public key Solana deposit accounts derive from; empty assigns none
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

	sol "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/api"
	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/chain/solana"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/mock"
	"github.com/darkodi/ferry/internal/rail/noah"
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
	dataKey, err := destination.ParseKey(os.Getenv("FERRY_DATA_KEY"))
	if err != nil {
		return fmt.Errorf("FERRY_DATA_KEY (create one with `openssl rand -base64 32`): %w", err)
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

	rl, err := railFromEnv(dev)
	if err != nil {
		return err
	}
	dests, err := destination.NewStore(pool, rl.Name(), dataKey)
	if err != nil {
		return err
	}
	payouts := payout.NewService(pool, rl, dests, house, time.Now)

	// In dev the relay, worker and resolver run in this process. The mock
	// rail lives in this process's memory, so with it they must; with
	// Noah they may also run as their own binaries (cmd/relay, cmd/worker).
	var background sync.WaitGroup
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer func() { stopBackground(); background.Wait() }()
	switch {
	case dev:
		if err := startBackground(bgCtx, &background, pool, payouts, log); err != nil {
			return err
		}
	case rl.Name() == "mock":
		log.Warn("payouts will stay held: the mock rail cannot serve a separate worker; use FERRY_ENV=dev or FERRY_RAIL=noah")
	default:
		log.Info("payouts are submitted by cmd/worker", "rail", rl.Name())
	}

	var hooks api.RailWebhooks
	if rl.Name() == "noah" {
		if hooks, err = noah.WebhooksFromEnv(os.Getenv); err != nil {
			return err
		}
	}

	addresses, err := depositAddresses(ctx, pool, log)
	if err != nil {
		return err
	}

	handler := api.New(api.Config{
		Pool:         pool,
		Payouts:      payouts,
		Destinations: dests,
		Idem:         idempotency.NewStore(pool, time.Now, 30*time.Second, log),
		House:        house,
		Addresses:    addresses,
		AdminKey:     adminKey,
		Dev:          dev,
		RailWebhooks: hooks,
		Log:          log,
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

// railFromEnv picks the payout rail: the in-memory mock (default; in dev it
// settles at once) or Noah.
func railFromEnv(dev bool) (rail.Rail, error) {
	switch name := os.Getenv("FERRY_RAIL"); name {
	case "", "mock":
		m := mock.New(time.Now)
		m.AutoSettle = dev
		return m, nil
	case "noah":
		return noah.FromEnv(os.Getenv, time.Now)
	default:
		return nil, fmt.Errorf("unknown FERRY_RAIL %q (want mock or noah)", name)
	}
}

// depositAddresses sets up deposit address assignment for every
// configured chain (FERRY_EVM_XPUB, FERRY_SOLANA_AUTHORITY) and gives
// existing customers an address on any newly configured chain. Both are
// public keys: the API can compute addresses but never move funds.
func depositAddresses(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*chain.Addresses, error) {
	var ds []chain.Deriver
	if xpub := os.Getenv("FERRY_EVM_XPUB"); xpub != "" {
		d, err := evm.NewDeriver(envOr("FERRY_EVM_CHAIN", "evm-local"), xpub)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	if auth := os.Getenv("FERRY_SOLANA_AUTHORITY"); auth != "" {
		pk, err := sol.PublicKeyFromBase58(auth)
		if err != nil {
			return nil, fmt.Errorf("FERRY_SOLANA_AUTHORITY: %w", err)
		}
		d, err := solana.NewDeriver(envOr("FERRY_SOLANA_CHAIN", "solana-local"), pk)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	if len(ds) == 0 {
		log.Warn("no chain configured (FERRY_EVM_XPUB, FERRY_SOLANA_AUTHORITY): new customers get no deposit address")
		return nil, nil
	}
	a := chain.NewAddresses(ds...)
	n, err := a.Backfill(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("backfill deposit addresses: %w", err)
	}
	if n > 0 {
		log.Info("assigned deposit addresses to existing customers", "count", n)
	}
	return a, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
