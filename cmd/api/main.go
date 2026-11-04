// Command api serves Ferry's HTTP API.
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL     Postgres connection string (required)
//	FERRY_ADMIN_KEY  operator key for admin routes, at least 32 characters (required)
//	FERRY_ENV        "dev" enables /v1/dev routes and an auto-settling mock rail
//	FERRY_ADDR       listen address (default 127.0.0.1:8080)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/api"
	"github.com/darkodi/ferry/internal/idempotency"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
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

	// M2 has only the mock rail; the Noah adapter replaces it in M5.
	rl := mock.New(time.Now)
	rl.AutoSettle = dev

	handler := api.New(api.Config{
		Pool:     pool,
		Payouts:  payout.NewService(pool, rl, house, time.Now),
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
