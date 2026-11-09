// Package solana reads USDC deposits from Solana (a local test validator
// or devnet) for chain.Watcher, derives customer deposit addresses from
// Ferry's authority public key, and creates those accounts on-chain.
//
// Everything is read at the "finalized" commitment: a finalized slot can
// no longer be rolled back, so the watcher credits Solana deposits as soon
// as it sees them (Confirmations = 0) and its reorg handling never fires.
package solana

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/chain"
)

// Agave RPC error codes (rpc-client-api/src/custom_error.rs).
const (
	errBlockNotAvailable          = -32004 // not produced or not finalized yet
	errSlotSkipped                = -32007
	errLongTermStorageSlotSkipped = -32009
)

// SkippedSlot is the hash recorded for a slot that has no block. A
// finalized slot that was skipped stays skipped, so the value is stable,
// which is all the watcher's hash comparison needs.
const SkippedSlot = "skipped"

// pageSize is the most signatures getSignaturesForAddress returns per call.
const pageSize = 1000

// Adapter implements chain.Adapter for one USDC mint on one Solana
// cluster. It only reads; account creation is the Provisioner's job.
type Adapter struct {
	name  string
	rpc   *rpc.Client
	mint  sol.PublicKey
	owner sol.PublicKey
}

// New returns an adapter named name (e.g. "solana-devnet") crediting
// increases of mint balances in watched token accounts whose token owner
// is owner (the deposit owner).
func New(name string, client *rpc.Client, mint, owner sol.PublicKey) *Adapter {
	return &Adapter{name: name, rpc: client, mint: mint, owner: owner}
}

// Chain returns the adapter's name.
func (a *Adapter) Chain() string { return a.name }

// Head returns the newest finalized slot.
func (a *Adapter) Head(ctx context.Context) (chain.Block, error) {
	slot, err := a.rpc.GetSlot(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return chain.Block{}, fmt.Errorf("solana: finalized slot: %w", err)
	}
	hash, err := a.BlockHash(ctx, slot)
	if err != nil {
		return chain.Block{}, err
	}
	return chain.Block{Number: slot, Hash: hash}, nil
}

// BlockHash returns the blockhash of finalized slot n, or SkippedSlot if
// no block was produced in it.
func (a *Adapter) BlockHash(ctx context.Context, n uint64) (string, error) {
	noRewards := false
	b, err := a.rpc.GetBlockWithOpts(ctx, n, &rpc.GetBlockOpts{
		TransactionDetails: rpc.TransactionDetailsNone,
		Rewards:            &noRewards,
		Commitment:         rpc.CommitmentFinalized,
	})
	if rpcErr := (*jsonrpc.RPCError)(nil); errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case errSlotSkipped, errLongTermStorageSlotSkipped:
			return SkippedSlot, nil
		case errBlockNotAvailable:
			return "", fmt.Errorf("solana: slot %d: %w", n, chain.ErrNotFound)
		}
	}
	if err != nil {
		return "", fmt.Errorf("solana: block %d: %w", n, err)
	}
	if b == nil {
		return "", fmt.Errorf("solana: slot %d: %w", n, chain.ErrNotFound)
	}
	return b.Blockhash.String(), nil
}

// ScanTransfers returns the USDC credits to watched token accounts in
// finalized slots from..to.
//
// For each watched account it lists the account's transaction signatures
// (newest first, paging back until it passes from), then reads each
// transaction once and compares token balances before and after it (see
// Credits). A transfer is identified by (signature, account index), stored
// as TxHash and LogIndex.
func (a *Adapter) ScanTransfers(ctx context.Context, from, to uint64, watched map[string]uuid.UUID) ([]chain.Transfer, error) {
	keys := make(map[sol.PublicKey]bool, len(watched))
	for addr := range watched {
		pk, err := sol.PublicKeyFromBase58(addr)
		if err != nil {
			continue // not a Solana address; never ours
		}
		keys[pk] = true
	}
	sigs := map[sol.Signature]uint64{}
	for pk := range keys {
		if err := a.signatures(ctx, pk, from, to, sigs); err != nil {
			return nil, err
		}
	}

	// Read in slot order so results are deterministic.
	type sigSlot struct {
		sig  sol.Signature
		slot uint64
	}
	order := make([]sigSlot, 0, len(sigs))
	for s, slot := range sigs {
		order = append(order, sigSlot{s, slot})
	}
	slices.SortFunc(order, func(x, y sigSlot) int {
		return cmp.Or(cmp.Compare(x.slot, y.slot), cmp.Compare(x.sig.String(), y.sig.String()))
	})

	hashes := map[uint64]string{}
	var out []chain.Transfer
	for _, s := range order {
		credits, slot, err := a.credits(ctx, s.sig, from, to, keys)
		if err != nil {
			return nil, err
		}
		if len(credits) == 0 {
			continue
		}
		hash, ok := hashes[slot]
		if !ok {
			if hash, err = a.BlockHash(ctx, slot); err != nil {
				return nil, err
			}
			hashes[slot] = hash
		}
		for _, c := range credits {
			out = append(out, chain.Transfer{
				TxHash:   s.sig.String(),
				LogIndex: uint64(c.Index),
				Block:    chain.Block{Number: slot, Hash: hash},
				To:       c.Account.String(),
				Amount:   c.Amount,
			})
		}
	}
	return out, nil
}

// signatures adds to sigs the successful transactions touching account in
// slots from..to.
func (a *Adapter) signatures(ctx context.Context, account sol.PublicKey, from, to uint64, sigs map[sol.Signature]uint64) error {
	limit := pageSize
	opts := &rpc.GetSignaturesForAddressOpts{Limit: &limit, Commitment: rpc.CommitmentFinalized}
	for {
		page, err := a.rpc.GetSignaturesForAddressWithOpts(ctx, account, opts)
		if err != nil {
			return fmt.Errorf("solana: signatures of %s: %w", account, err)
		}
		for _, s := range page {
			switch {
			case s.Slot > to: // newer than this range; a later round reads it
			case s.Slot < from:
				return nil // newest first: everything after this is older
			case s.Err == nil:
				sigs[s.Signature] = s.Slot
			}
		}
		if len(page) < limit {
			return nil
		}
		opts.Before = page[len(page)-1].Signature
	}
}

// credits reads one finalized transaction and returns the USDC credits to
// watched accounts in it, and its slot. The node's answer is checked: it
// must be the transaction asked for, in the scanned range, and successful.
func (a *Adapter) credits(ctx context.Context, sig sol.Signature, from, to uint64, watched map[sol.PublicKey]bool) ([]Credit, uint64, error) {
	v0 := rpc.MaxSupportedTransactionVersion0
	res, err := a.rpc.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Encoding:                       sol.EncodingBase64,
		Commitment:                     rpc.CommitmentFinalized,
		MaxSupportedTransactionVersion: &v0,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("solana: transaction %s: %w", sig, err)
	}
	if res.Meta == nil || res.Meta.Err != nil || res.Transaction == nil || res.Slot < from || res.Slot > to {
		return nil, 0, nil
	}
	tx, err := res.Transaction.GetTransaction()
	if err != nil {
		return nil, 0, fmt.Errorf("solana: decode transaction %s: %w", sig, err)
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0] != sig {
		return nil, 0, fmt.Errorf("solana: node returned a different transaction for %s", sig)
	}
	// accountIndex in token balances counts static keys first, then the
	// addresses loaded from lookup tables: writable, then read-only.
	keys := slices.Concat(tx.Message.AccountKeys, res.Meta.LoadedAddresses.Writable, res.Meta.LoadedAddresses.ReadOnly)
	cs := Credits(keys, res.Meta.PreTokenBalances, res.Meta.PostTokenBalances, a.mint, a.owner,
		func(k sol.PublicKey) bool { return watched[k] })
	return cs, res.Slot, nil
}
