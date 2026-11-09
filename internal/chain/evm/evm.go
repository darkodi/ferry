// Package evm reads USDC deposits from an EVM chain (Anvil locally,
// Sepolia or Base Sepolia on testnet) for chain.Watcher, and derives
// customer deposit addresses from an extended public key.
package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/chain"
	"github.com/darkodi/ferry/internal/ledger"
)

// TransferTopic is keccak256("Transfer(address,address,uint256)"), the
// first topic of every ERC-20 Transfer log.
var TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// Client is the part of an Ethereum RPC client the adapter uses. Both
// *ethclient.Client and the simulated backend's client satisfy it.
type Client interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
}

// Adapter implements chain.Adapter for one USDC contract on one EVM chain.
type Adapter struct {
	name   string
	client Client
	usdc   common.Address
}

// New returns an adapter named name (e.g. "evm-sepolia") reading the USDC
// contract at usdc.
func New(name string, client Client, usdc common.Address) *Adapter {
	return &Adapter{name: name, client: client, usdc: usdc}
}

// Chain returns the adapter's name.
func (a *Adapter) Chain() string { return a.name }

// Head returns the latest block. The watcher applies confirmation depth.
func (a *Adapter) Head(ctx context.Context) (chain.Block, error) {
	h, err := a.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return chain.Block{}, fmt.Errorf("evm: latest header: %w", err)
	}
	return chain.Block{Number: h.Number.Uint64(), Hash: h.Hash().Hex()}, nil
}

// BlockHash returns the hash of block n on the current canonical chain.
func (a *Adapter) BlockHash(ctx context.Context, n uint64) (string, error) {
	h, err := a.client.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if errors.Is(err, ethereum.NotFound) {
		return "", fmt.Errorf("evm: block %d: %w", n, chain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("evm: header %d: %w", n, err)
	}
	return h.Hash().Hex(), nil
}

// ScanTransfers returns USDC Transfer logs to watched addresses in blocks
// from..to. The RPC node is not trusted: every log is re-checked for the
// contract address, event signature, recipient and amount range.
func (a *Adapter) ScanTransfers(ctx context.Context, from, to uint64, watched map[string]uuid.UUID) ([]chain.Transfer, error) {
	recipients := make([]common.Hash, 0, len(watched))
	for addr := range watched {
		recipients = append(recipients, common.BytesToHash(common.HexToAddress(addr).Bytes()))
	}
	logs, err := a.client.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{a.usdc},
		// topic 0: event; topic 1: from (any); topic 2: to (one of ours)
		Topics: [][]common.Hash{{TransferTopic}, nil, recipients},
	})
	if err != nil {
		return nil, fmt.Errorf("evm: filter logs %d..%d: %w", from, to, err)
	}
	var out []chain.Transfer
	for _, l := range logs {
		t, ok := a.decode(l, from, to)
		if !ok {
			continue
		}
		if _, ours := watched[t.To]; ours {
			out = append(out, t)
		}
	}
	return out, nil
}

// decode checks one log and turns it into a transfer. It rejects logs from
// other contracts or events, logs outside the asked range, removed (reorged)
// logs, and amounts that are zero or do not fit in int64 minor units.
func (a *Adapter) decode(l types.Log, from, to uint64) (chain.Transfer, bool) {
	if l.Removed || l.Address != a.usdc || len(l.Topics) != 3 || l.Topics[0] != TransferTopic {
		return chain.Transfer{}, false
	}
	if l.BlockNumber < from || l.BlockNumber > to || len(l.Data) != 32 {
		return chain.Transfer{}, false
	}
	amount, ok := ParseAmount(l.Data)
	if !ok {
		return chain.Transfer{}, false
	}
	return chain.Transfer{
		TxHash:   l.TxHash.Hex(),
		LogIndex: uint64(l.Index),
		Block:    chain.Block{Number: l.BlockNumber, Hash: l.BlockHash.Hex()},
		To:       common.BytesToAddress(l.Topics[2].Bytes()).Hex(),
		Amount:   amount,
	}, true
}

// ParseAmount decodes a uint256 token amount (32 bytes, big-endian) into
// USDC minor units. It reports false for zero and for values above the
// ledger's int64 range: those are refused, never truncated.
func ParseAmount(data []byte) (ledger.Amount, bool) {
	if len(data) != 32 {
		return 0, false
	}
	v := new(big.Int).SetBytes(data)
	if v.Sign() <= 0 || !v.IsInt64() {
		return 0, false
	}
	return ledger.Amount(v.Int64()), true
}
