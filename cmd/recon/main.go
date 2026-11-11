// Command recon reconciles Ferry against the chains, the rail and itself
// (internal/recon) and records the breaks it finds. It reads only: chain
// RPC, the rail's API (read endpoints) and the database. It holds no
// private key and no data key, and never changes the ledger.
//
// Run it on a schedule (cron, or an ECS scheduled task in M7), or locally
// with -every:
//
//	go run ./cmd/recon                 # one run, exit
//	go run ./cmd/recon -every 1m       # run every minute until stopped
//	go run ./cmd/recon -window 48h     # rail window: the last 48 hours (default)
//
// Configuration comes from the environment (see .env.example). A check
// runs only if its source is configured:
//
//	DATABASE_URL                                 required
//	FERRY_EVM_RPC_URL, FERRY_EVM_USDC_ADDRESS    chain check for FERRY_EVM_CHAIN (FERRY_EVM_CONFIRMATIONS)
//	FERRY_SOLANA_RPC_URL                         chain check for FERRY_SOLANA_CHAIN
//	FERRY_RAIL=noah and NOAH_*                   rail check
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/chain/solana"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/recon"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("recon stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	every := flag.Duration("every", 0, "run repeatedly at this interval (0: run once)")
	window := flag.Duration("window", 48*time.Hour, "rail check covers transactions created this long before the run")
	settle := flag.Duration("settle", 10*time.Minute, "leave the newest transactions out of the rail window for this long")
	flag.Parse()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

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

	checkers := []recon.Checker{recon.NewInternal(pool)}
	if rpcURL, usdc := os.Getenv("FERRY_EVM_RPC_URL"), os.Getenv("FERRY_EVM_USDC_ADDRESS"); rpcURL != "" && common.IsHexAddress(usdc) {
		client, err := ethclient.DialContext(ctx, rpcURL)
		if err != nil {
			return fmt.Errorf("dial %s: %w", rpcURL, err)
		}
		conf, err := strconv.ParseUint(envOr("FERRY_EVM_CONFIRMATIONS", "12"), 10, 64)
		if err != nil {
			return fmt.Errorf("FERRY_EVM_CONFIRMATIONS: %w", err)
		}
		checkers = append(checkers, recon.NewChain(pool, evm.NewBalances(envOr("FERRY_EVM_CHAIN", "evm-local"), client, common.HexToAddress(usdc), conf)))
	}
	if rpcURL := os.Getenv("FERRY_SOLANA_RPC_URL"); rpcURL != "" {
		checkers = append(checkers, recon.NewChain(pool, solana.NewBalances(envOr("FERRY_SOLANA_CHAIN", "solana-local"), rpc.New(rpcURL))))
	}
	if os.Getenv("FERRY_RAIL") == "noah" {
		nr, err := noah.FromEnv(os.Getenv, time.Now)
		if err != nil {
			return err
		}
		checkers = append(checkers, recon.NewRail(pool, nr.Name(), nr, house))
	}
	names := make([]string, len(checkers))
	for i, c := range checkers {
		names[i] = c.Check() + ":" + c.Scope()
	}
	log.Info("recon configured", "checks", names, "window", window.String())

	r := recon.New(pool, log, checkers...)
	for {
		now := time.Now().UTC()
		rep, err := r.Run(ctx, recon.Window{From: now.Add(-*window), To: now.Add(-*settle)})
		switch {
		case errors.Is(err, recon.ErrBusy):
			log.Info("another recon run is in progress; skipping")
		case err != nil:
			return err
		default:
			open, err := recon.OpenCount(ctx, pool)
			if err != nil {
				return err
			}
			log.Info("recon run", "run_id", rep.RunID, "completed", rep.Completed, "failed", rep.Failed,
				"breaks_seen", rep.Breaks, "new", rep.New, "cleared", rep.Cleared, "open", open)
			if *every == 0 && len(rep.Failed) > 0 {
				return fmt.Errorf("checks could not run: %v", rep.Failed)
			}
		}
		if *every == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*every):
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
