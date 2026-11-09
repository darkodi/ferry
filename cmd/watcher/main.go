// Command watcher detects USDC deposits on one chain and credits them once
// they are final. Run one per chain; a second copy for the same chain
// stands by until the first stops (Postgres advisory lock).
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL             Postgres connection string (required)
//	FERRY_CHAIN              "evm" (Solana arrives in the next step)
//	FERRY_EVM_CHAIN          chain name in the database (default evm-local); must match cmd/api
//	FERRY_EVM_RPC_URL        JSON-RPC endpoint (required)
//	FERRY_EVM_USDC_ADDRESS   USDC contract to watch (required)
//	FERRY_EVM_CONFIRMATIONS  blocks before a deposit is credited (default 12)
//	FERRY_EVM_START_BLOCK    first block to scan on a fresh database (default: current head)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/ledger"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("watcher stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	if c := os.Getenv("FERRY_CHAIN"); c != "evm" {
		return fmt.Errorf("FERRY_CHAIN must be \"evm\", got %q", c)
	}

	// Cancelled on Ctrl-C or SIGTERM: the current round finishes first.
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

	w, err := evmWatcher(ctx, pool, house, log)
	if err != nil {
		return err
	}
	return w.Run(ctx, 2*time.Second)
}

func evmWatcher(ctx context.Context, pool *pgxpool.Pool, house ledger.HouseAccounts, log *slog.Logger) (*chain.Watcher, error) {
	name := envOr("FERRY_EVM_CHAIN", "evm-local")
	rpc := os.Getenv("FERRY_EVM_RPC_URL")
	usdc := os.Getenv("FERRY_EVM_USDC_ADDRESS")
	if rpc == "" || !common.IsHexAddress(usdc) {
		return nil, errors.New("FERRY_EVM_RPC_URL and a valid FERRY_EVM_USDC_ADDRESS are required")
	}
	confirmations, err := strconv.ParseUint(envOr("FERRY_EVM_CONFIRMATIONS", "12"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("FERRY_EVM_CONFIRMATIONS: %w", err)
	}
	start, err := strconv.ParseUint(envOr("FERRY_EVM_START_BLOCK", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("FERRY_EVM_START_BLOCK: %w", err)
	}
	client, err := ethclient.DialContext(ctx, rpc)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", rpc, err)
	}
	w := chain.NewWatcher(pool, evm.New(name, client, common.HexToAddress(usdc)), house, log)
	w.Confirmations = confirmations
	w.StartBlock = start
	log.Info("watcher configured", "chain", name, "usdc", usdc, "confirmations", confirmations)
	return w, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
