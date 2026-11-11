// Command watcher detects USDC deposits on one chain and credits them once
// they are final. Run one per chain; a second copy for the same chain
// stands by until the first stops (Postgres advisory lock).
//
// Configuration comes from the environment (see .env.example):
//
//	DATABASE_URL             Postgres connection string (required)
//	FERRY_CHAIN              "evm" or "solana"
//
// EVM:
//
//	FERRY_EVM_CHAIN          chain name in the database (default evm-local); must match cmd/api
//	FERRY_EVM_RPC_URL        JSON-RPC endpoint (required)
//	FERRY_EVM_USDC_ADDRESS   USDC contract to watch (required)
//	FERRY_EVM_CONFIRMATIONS  blocks before a deposit is credited (default 12)
//	FERRY_EVM_START_BLOCK    first block to scan on a fresh database (default: current head)
//
// Solana (reads finalized slots only, so deposits are credited on sight):
//
//	FERRY_SOLANA_CHAIN             chain name in the database (default solana-local); must match cmd/api
//	FERRY_SOLANA_RPC_URL           JSON-RPC endpoint (required)
//	FERRY_SOLANA_USDC_MINT         USDC mint (required)
//	FERRY_SOLANA_AUTHORITY_KMS_KEY_ID  the authority as an AWS KMS Ed25519 key (it never leaves KMS), or
//	FERRY_SOLANA_AUTHORITY_KEYPAIR path to the authority's keypair file (solana-keygen JSON, local);
//	                               it pays for and signs deposit account creation, nothing else
//	FERRY_SOLANA_DEPOSIT_OWNER     public key that owns the deposit token accounts (required); its
//	                               private key moves funds and is never given to Ferry
//	FERRY_SOLANA_START_SLOT        first slot to scan on a fresh database (default: current finalized slot)
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
	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/chain/evm"
	"github.com/darkodi/ferry/internal/chain/solana"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/awskms"
	"github.com/darkodi/ferry/internal/platform/obs"
)

func main() {
	log := obs.NewLogger()
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
	newWatcher := map[string]func(context.Context, *pgxpool.Pool, ledger.HouseAccounts, *slog.Logger) (*chain.Watcher, error){
		"evm":    evmWatcher,
		"solana": solanaWatcher,
	}[os.Getenv("FERRY_CHAIN")]
	if newWatcher == nil {
		return fmt.Errorf("FERRY_CHAIN must be \"evm\" or \"solana\", got %q", os.Getenv("FERRY_CHAIN"))
	}

	// Cancelled on Ctrl-C or SIGTERM: the current round finishes first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTraces, err := obs.Setup(ctx, "ferry-watcher")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTraces(context.Background()) }()
	if err := obs.Serve(ctx, envOr("FERRY_METRICS_ADDR", map[string]string{"evm": "127.0.0.1:9104", "solana": "127.0.0.1:9105"}[os.Getenv("FERRY_CHAIN")]), log); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	house, err := ledger.OpenHouseAccounts(ctx, pool)
	if err != nil {
		return fmt.Errorf("open house accounts: %w", err)
	}

	w, err := newWatcher(ctx, pool, house, log)
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

func solanaWatcher(ctx context.Context, pool *pgxpool.Pool, house ledger.HouseAccounts, log *slog.Logger) (*chain.Watcher, error) {
	name := envOr("FERRY_SOLANA_CHAIN", "solana-local")
	url := os.Getenv("FERRY_SOLANA_RPC_URL")
	if url == "" {
		return nil, errors.New("FERRY_SOLANA_RPC_URL is required")
	}
	mint, err := sol.PublicKeyFromBase58(os.Getenv("FERRY_SOLANA_USDC_MINT"))
	if err != nil {
		return nil, fmt.Errorf("FERRY_SOLANA_USDC_MINT: %w", err)
	}
	signer, err := authoritySigner(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := sol.PublicKeyFromBase58(os.Getenv("FERRY_SOLANA_DEPOSIT_OWNER"))
	if err != nil {
		return nil, fmt.Errorf("FERRY_SOLANA_DEPOSIT_OWNER: %w", err)
	}
	start, err := strconv.ParseUint(envOr("FERRY_SOLANA_START_SLOT", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("FERRY_SOLANA_START_SLOT: %w", err)
	}
	client := rpc.New(url)
	prov, err := solana.NewProvisionerWithSigner(client, signer, mint, owner)
	if err != nil {
		return nil, err
	}
	w := chain.NewWatcher(pool, solana.New(name, client, mint, owner), house, log)
	w.Confirmations = 0 // finalized slots cannot be rolled back
	w.StartBlock = start
	w.Provisioner = prov
	log.Info("watcher configured", "chain", name, "mint", mint.String(), "authority", prov.Authority().String(), "deposit_owner", owner.String())
	return w, nil
}

// authoritySigner returns the Solana authority: a key in AWS KMS
// (FERRY_SOLANA_AUTHORITY_KMS_KEY_ID, ECC_NIST_EDWARDS25519) or, locally,
// a keypair file (FERRY_SOLANA_AUTHORITY_KEYPAIR). Exactly one is set.
func authoritySigner(ctx context.Context) (solana.TxSigner, error) {
	keyPath, kmsKey := os.Getenv("FERRY_SOLANA_AUTHORITY_KEYPAIR"), os.Getenv("FERRY_SOLANA_AUTHORITY_KMS_KEY_ID")
	switch {
	case keyPath != "" && kmsKey != "":
		return nil, errors.New("set FERRY_SOLANA_AUTHORITY_KEYPAIR or FERRY_SOLANA_AUTHORITY_KMS_KEY_ID, not both")
	case kmsKey != "":
		api, err := awskms.New(ctx, os.Getenv("FERRY_KMS_ENDPOINT"))
		if err != nil {
			return nil, err
		}
		ks, err := awskms.NewEd25519Signer(ctx, api, kmsKey)
		if err != nil {
			return nil, err
		}
		return solana.KMSSigner(ks), nil
	case keyPath != "":
		key, err := sol.PrivateKeyFromSolanaKeygenFile(keyPath)
		if err != nil {
			// The error names the file, never its contents.
			return nil, fmt.Errorf("read authority keypair %s: %w", keyPath, err)
		}
		return solana.KeySigner(key), nil
	default:
		return nil, errors.New("FERRY_SOLANA_AUTHORITY_KEYPAIR or FERRY_SOLANA_AUTHORITY_KMS_KEY_ID is required")
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
