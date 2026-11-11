package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/darkodi/ferry/internal/ledger"
)

// balanceOfSelector is the first 4 bytes of keccak256("balanceOf(address)").
var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31}

// CallClient is the part of an Ethereum client the balance reader uses.
type CallClient interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// Balances reads USDC balances of deposit addresses for reconciliation.
type Balances struct {
	name          string
	client        CallClient
	usdc          common.Address
	confirmations uint64
}

// NewBalances returns a reader for the USDC contract on chain name.
// Balances are read at a block at least confirmations deep.
func NewBalances(name string, client CallClient, usdc common.Address, confirmations uint64) *Balances {
	return &Balances{name: name, client: client, usdc: usdc, confirmations: confirmations}
}

// Chain returns the chain name.
func (b *Balances) Chain() string { return b.name }

// Point is the block balances are read at: the watcher's cursor, or the
// newest confirmed block if that is lower. Deposits Ferry recorded in
// later blocks are left out of the comparison, so an unscanned or
// unconfirmed transfer is not a break.
func (b *Balances) Point(ctx context.Context, cursor uint64) (uint64, error) {
	h, err := b.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("evm: latest header: %w", err)
	}
	head := h.Number.Uint64()
	if head < b.confirmations {
		return 0, nil
	}
	return min(cursor, head-b.confirmations), nil
}

// Balance calls USDC.balanceOf(address) at block point.
func (b *Balances) Balance(ctx context.Context, address string, point uint64) (ledger.Amount, error) {
	if !common.IsHexAddress(address) {
		return 0, fmt.Errorf("evm: %q is not an address", address)
	}
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(common.HexToAddress(address).Bytes(), 32)...)
	out, err := b.client.CallContract(ctx, ethereum.CallMsg{To: &b.usdc, Data: data}, new(big.Int).SetUint64(point))
	if err != nil {
		return 0, fmt.Errorf("evm: balanceOf(%s) at %d: %w", address, point, err)
	}
	if len(out) != 32 {
		return 0, fmt.Errorf("evm: balanceOf(%s) returned %d bytes", address, len(out))
	}
	v := new(big.Int).SetBytes(out)
	if !v.IsInt64() {
		return 0, errors.New("evm: balance above the ledger's int64 range")
	}
	return ledger.Amount(v.Int64()), nil
}
