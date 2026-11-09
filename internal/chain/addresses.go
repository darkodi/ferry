package chain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain/chaindb"
	"github.com/darkodi/ferry/internal/platform/db"
)

// Addresses assigns deposit addresses to customers on every configured
// chain.
type Addresses struct {
	derivers []Deriver
}

// NewAddresses returns an assigner for the given chains. With none, Assign
// does nothing.
func NewAddresses(d ...Deriver) *Addresses { return &Addresses{derivers: d} }

// Address is one of a customer's deposit addresses.
type Address struct {
	Chain     string
	Address   string
	Status    string
	CreatedAt time.Time
}

// Assign gives a new customer one derivation index and its address on each
// chain, inside the caller's transaction (customer creation).
func (a *Addresses) Assign(ctx context.Context, tx pgx.Tx, customerID uuid.UUID) error {
	if a == nil || len(a.derivers) == 0 {
		return nil
	}
	q := chaindb.New(tx)
	idx, err := q.NextDepositIndex(ctx)
	if err != nil {
		return fmt.Errorf("chain: next address index: %w", err)
	}
	for _, d := range a.derivers {
		if err := insert(ctx, q, d, customerID, idx); err != nil {
			return err
		}
	}
	return nil
}

// Backfill assigns addresses to customers created before a chain was
// configured. A customer reuses its existing index where it has one.
func (a *Addresses) Backfill(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	if a == nil {
		return 0, nil
	}
	n := 0
	for _, d := range a.derivers {
		rows, err := chaindb.New(pool).CustomersWithoutAddress(ctx, d.Chain())
		if err != nil {
			return n, fmt.Errorf("chain: customers without %s address: %w", d.Chain(), err)
		}
		for _, r := range rows {
			err := db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				q := chaindb.New(tx)
				idx := r.ExistingIndex
				if idx < 0 {
					var err error
					if idx, err = q.NextDepositIndex(ctx); err != nil {
						return fmt.Errorf("chain: next address index: %w", err)
					}
				}
				return insert(ctx, q, d, r.ID, idx)
			})
			if err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func insert(ctx context.Context, q *chaindb.Queries, d Deriver, customerID uuid.UUID, idx int64) error {
	addr, status, err := d.Derive(uint64(idx)) //nolint:gosec // from a sequence with MINVALUE 0
	if err != nil {
		return fmt.Errorf("chain: derive %s address %d: %w", d.Chain(), idx, err)
	}
	err = q.InsertDepositAddress(ctx, chaindb.InsertDepositAddressParams{
		CustomerID: customerID, Chain: d.Chain(), Address: addr, DerivationIndex: idx, Status: status,
	})
	if err != nil {
		return fmt.Errorf("chain: store %s address: %w", d.Chain(), err)
	}
	return nil
}

// CustomerAddresses lists a customer's deposit addresses.
func CustomerAddresses(ctx context.Context, q chaindb.DBTX, customerID uuid.UUID) ([]Address, error) {
	rows, err := chaindb.New(q).CustomerAddresses(ctx, customerID)
	if err != nil {
		return nil, fmt.Errorf("chain: customer addresses: %w", err)
	}
	out := make([]Address, len(rows))
	for i, r := range rows {
		out[i] = Address{Chain: r.Chain, Address: r.Address, Status: r.Status, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// Deposit is a detected deposit as shown to its customer.
type Deposit struct {
	ID          uuid.UUID
	Chain       string
	TxHash      string
	LogIndex    int64
	BlockNumber int64
	Address     string
	Amount      int64
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CustomerDeposits lists a customer's most recent deposits, newest first.
func CustomerDeposits(ctx context.Context, q chaindb.DBTX, customerID uuid.UUID, limit int32) ([]Deposit, error) {
	rows, err := chaindb.New(q).CustomerDeposits(ctx, chaindb.CustomerDepositsParams{CustomerID: customerID, MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("chain: customer deposits: %w", err)
	}
	out := make([]Deposit, len(rows))
	for i, r := range rows {
		out[i] = Deposit{
			ID: r.ID, Chain: r.Chain, TxHash: r.TxHash, LogIndex: r.LogIndex, BlockNumber: r.BlockNumber,
			Address: r.Address, Amount: r.Amount, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
	}
	return out, nil
}
