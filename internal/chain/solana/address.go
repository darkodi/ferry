package solana

import (
	"fmt"
	"strconv"

	sol "github.com/gagliardetto/solana-go"
)

// SeedPrefix starts every deposit account's seed: customer index i owns the
// account created with seed "ferry-<i>".
const SeedPrefix = "ferry-"

// Seed returns the account seed for a customer derivation index.
func Seed(index uint64) string { return SeedPrefix + strconv.FormatUint(index, 10) }

// DepositAddress is the deposit token account for index:
// createWithSeed(authority, "ferry-<index>", Token program). It is a plain
// hash of public values, so the API can compute it without any key; only
// the authority can create the account there (the system program requires
// the base's signature), so nobody else can claim the address first.
func DepositAddress(authority sol.PublicKey, index uint64) (sol.PublicKey, error) {
	addr, err := sol.CreateWithSeed(authority, Seed(index), sol.TokenProgramID)
	if err != nil {
		return sol.PublicKey{}, fmt.Errorf("solana: deposit address %d: %w", index, err)
	}
	return addr, nil
}

// Deriver computes customer deposit addresses from the authority's public
// key. It implements chain.Deriver.
type Deriver struct {
	name      string
	authority sol.PublicKey
}

// NewDeriver returns a deriver for the chain named name (e.g.
// "solana-devnet") whose accounts are based on authority.
func NewDeriver(name string, authority sol.PublicKey) (*Deriver, error) {
	if authority.IsZero() {
		return nil, fmt.Errorf("solana: authority public key is empty")
	}
	return &Deriver{name: name, authority: authority}, nil
}

// Chain returns the chain name.
func (d *Deriver) Chain() string { return d.name }

// Derive returns the address for index. The token account does not exist
// yet, so the status is "provisioning" until the watcher creates it.
func (d *Deriver) Derive(index uint64) (string, string, error) {
	addr, err := DepositAddress(d.authority, index)
	if err != nil {
		return "", "", fmt.Errorf("solana: derive index %d: %w", index, err)
	}
	return addr.String(), "provisioning", nil
}
