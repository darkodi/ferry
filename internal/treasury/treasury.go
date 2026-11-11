// Package treasury books money Ferry moves between its own accounts:
// for now, USDC arriving in Ferry's rail balance (a treasury top-up).
//
// The ledger's noah_prefunded account only goes down when payouts settle;
// a top-up is what brings it back up. The operator names the rail's
// transaction, and Ferry fetches it from the rail and books exactly that
// amount, once. The operator cannot type an amount.
package treasury

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/rail"
)

// Errors.
var (
	// ErrNotTopup: the rail transaction exists but is not a settled
	// incoming credit that can be a top-up.
	ErrNotTopup = errors.New("treasury: not a settled incoming credit")
	// ErrNotFound: the rail has no such transaction.
	ErrNotFound = errors.New("treasury: rail transaction not found")
)

// Service records treasury top-ups.
type Service struct {
	pool     *pgxpool.Pool
	railName string
	src      rail.Reconcilable
	currency string
	house    ledger.HouseAccounts
}

// New returns the service for one rail; currency is the rail's code for
// USDC (USDC_TEST in Noah's sandbox).
func New(pool *pgxpool.Pool, railName string, src rail.Reconcilable, currency string, house ledger.HouseAccounts) *Service {
	return &Service{pool: pool, railName: railName, src: src, currency: currency, house: house}
}

// Topup is a booked top-up.
type Topup struct {
	RailTxID string
	Amount   ledger.Amount
	// New is false when the top-up had been booked before (nothing
	// posted this time).
	New bool
}

// Record books the rail transaction id as a treasury top-up: debit
// noah_prefunded, credit chain_omnibus (the USDC came from Ferry's
// on-chain treasury). The transaction must be incoming, settled, in the
// rail's USDC, and not a return of a payout (those are bank returns).
// Recording the same transaction again changes nothing.
func (s *Service) Record(ctx context.Context, railTxID string) (Topup, error) {
	if railTxID == "" || len(railTxID) > 100 {
		return Topup{}, fmt.Errorf("%w: transaction id must be 1-100 characters", ErrNotFound)
	}
	r, found, err := s.src.Record(ctx, railTxID)
	if err != nil {
		return Topup{}, fmt.Errorf("treasury: fetch %s: %w", railTxID, err)
	}
	if !found {
		return Topup{}, fmt.Errorf("%w: %s", ErrNotFound, railTxID)
	}
	switch {
	case r.Direction != "In":
		return Topup{}, fmt.Errorf("%w: %s is %s", ErrNotTopup, railTxID, r.Direction)
	case r.Reverses != "":
		return Topup{}, fmt.Errorf("%w: %s returns payout transaction %s (a bank return, booked by the payout)", ErrNotTopup, railTxID, r.Reverses)
	case r.Status != rail.StatusSettled:
		return Topup{}, fmt.Errorf("%w: %s is %s", ErrNotTopup, railTxID, r.Status)
	case r.Currency != s.currency:
		return Topup{}, fmt.Errorf("%w: %s is in %s, not %s", ErrNotTopup, railTxID, r.Currency, s.currency)
	case r.Amount <= 0:
		return Topup{}, fmt.Errorf("%w: %s has no positive amount", ErrNotTopup, railTxID)
	}
	t := Topup{RailTxID: railTxID, Amount: r.Amount, New: true}
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := ledger.PostEntry(ctx, tx, ledger.TreasuryTopup(s.railName+":"+railTxID, s.house, r.Amount))
		if errors.Is(err, ledger.ErrDuplicateEntry) {
			t.New = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("treasury: post top-up: %w", err)
		}
		return nil
	})
	return t, err
}
