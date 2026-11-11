package solana

import (
	"context"
	"errors"
	"fmt"
	"math"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/darkodi/ferry/internal/ledger"
)

// Balances reads the USDC balances of deposit token accounts for
// reconciliation, at the finalized commitment.
type Balances struct {
	name string
	rpc  *rpc.Client
}

// NewBalances returns a reader for chain name.
func NewBalances(name string, client *rpc.Client) *Balances {
	return &Balances{name: name, rpc: client}
}

// Chain returns the chain name.
func (b *Balances) Chain() string { return b.name }

// Point returns math.MaxInt64: Solana balances can only be read now
// (finalized), so every recorded deposit counts. A finalized transfer the watcher has not
// scanned yet shows as a break that the next run clears.
func (b *Balances) Point(context.Context, uint64) (uint64, error) { return math.MaxInt64, nil }

// Balance returns the token account's amount; an account not created yet
// holds nothing.
func (b *Balances) Balance(ctx context.Context, address string, _ uint64) (ledger.Amount, error) {
	pk, err := sol.PublicKeyFromBase58(address)
	if err != nil {
		return 0, fmt.Errorf("solana: %q is not an address: %w", address, err)
	}
	if _, err := b.rpc.GetAccountInfoWithOpts(ctx, pk, &rpc.GetAccountInfoOpts{Commitment: rpc.CommitmentFinalized}); errors.Is(err, rpc.ErrNotFound) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("solana: account %s: %w", address, err)
	}
	res, err := b.rpc.GetTokenAccountBalance(ctx, pk, rpc.CommitmentFinalized)
	if err != nil {
		return 0, fmt.Errorf("solana: token balance of %s: %w", address, err)
	}
	v, ok := ParseAmount(res.Value)
	if !ok || v > math.MaxInt64 {
		return 0, fmt.Errorf("solana: token balance of %s is not a valid amount", address)
	}
	return ledger.Amount(v), nil
}
