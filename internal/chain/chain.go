// Package chain watches blockchains for USDC deposits to customer
// addresses and credits them once they are final.
//
// Chain specifics live behind Adapter (evm/, solana/); everything else —
// the scan cursor, reorg handling, confirmation and ledger posting — is in
// Watcher and is the same for every chain.
package chain

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/ledger"
)

// Block identifies a block (EVM) or slot (Solana).
type Block struct {
	Number uint64
	Hash   string
}

// Transfer is a USDC transfer to a watched address, as read from the chain.
type Transfer struct {
	TxHash   string
	LogIndex uint64 // position within the transaction or block; with TxHash it identifies the transfer
	Block    Block
	To       string // canonical form of the address, as in deposit_addresses
	Amount   ledger.Amount
}

// Adapter is the read side of one chain. The design's SendUSDC and
// TxStatus arrive with the treasury sweep.
type Adapter interface {
	// Chain is the name stored in the database, e.g. "evm-sepolia".
	Chain() string
	// Head is the newest block the adapter is willing to read: the tip for
	// EVM (confirmation depth is applied by the Watcher), the newest
	// finalized slot for Solana.
	Head(ctx context.Context) (Block, error)
	// BlockHash is the hash of block n on the chain as it is now.
	BlockHash(ctx context.Context, n uint64) (string, error)
	// ScanTransfers returns the USDC transfers in blocks from..to
	// (inclusive) whose recipient is in watched (keyed by canonical
	// address). Malformed or out-of-range amounts are skipped, never
	// rounded.
	ScanTransfers(ctx context.Context, from, to uint64, watched map[string]uuid.UUID) ([]Transfer, error)
}

// ErrNotFound means the chain has no block at that height (yet).
var ErrNotFound = errors.New("chain: block not found")

// Deriver computes a customer's deposit address on one chain from public
// key material only. It never needs a private key.
type Deriver interface {
	Chain() string
	// Derive returns the address for index and its initial status:
	// "active", or "provisioning" if an on-chain account must be created
	// first.
	Derive(index uint64) (address, status string, err error)
}
