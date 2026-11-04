package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/darkodi/ferry/internal/ledger/ledgerdb"
)

// DB is anything that can run ledger queries: a pgx.Tx, *pgx.Conn or
// *pgxpool.Pool. PostEntry needs a transaction; the read helpers accept any.
type DB = ledgerdb.DBTX

// Constraint names from migrations/00001_ledger.sql that map to errors.
const (
	conReversesUnique = "journal_entries_reverses_entry_id_key"
	conReversesFK     = "journal_entries_reverses_entry_id_fkey"
)

// PostEntry writes e inside tx: the journal entry, its postings and the new
// balance of every account it touches. It does not commit; the caller
// commits together with its own state change and outbox row (design rule
// "write first, act second").
//
// Steps:
//  1. Validate e (no database access).
//  2. Insert the journal entry. If the idempotency key exists, return the
//     existing id and ErrDuplicateEntry; nothing else is written and tx
//     stays usable, so a replayed step can treat this as "already done".
//  3. Lock the touched account rows in ascending id order, so two entries
//     touching the same accounts cannot deadlock.
//  4. Compute the new balances; refuse with ErrInsufficientFunds if a
//     customer account would go negative.
//  5. Insert the postings and store the new balances.
//
// On any error other than ErrDuplicateEntry the caller must roll back tx.
func PostEntry(ctx context.Context, tx pgx.Tx, e Entry) (EntryID, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	q := ledgerdb.New(tx)

	// 2. Insert the entry, deduplicating on its idempotency key.
	var reverses pgtype.Int8
	if e.Reverses != nil {
		reverses = pgtype.Int8{Int64: int64(*e.Reverses), Valid: true}
	}
	id, err := q.InsertEntry(ctx, ledgerdb.InsertEntryParams{
		IdempotencyKey:  e.IdempotencyKey,
		Type:            string(e.Type),
		RefID:           e.RefID,
		ReversesEntryID: reverses,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		existing, err := q.EntryIDByKey(ctx, e.IdempotencyKey)
		if err != nil {
			return 0, fmt.Errorf("ledger: look up duplicate entry: %w", err)
		}
		return EntryID(existing), fmt.Errorf("%w: key %q", ErrDuplicateEntry, e.IdempotencyKey)
	case err != nil:
		return 0, mapInsertEntryError(err, e)
	}
	entryID := EntryID(id)

	// 3. Lock accounts, ascending id.
	ids := make([]int64, 0, len(e.Postings))
	for _, p := range e.Postings {
		ids = append(ids, int64(p.Account))
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	rows, err := q.LockAccounts(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("ledger: lock accounts: %w", err)
	}
	if len(rows) != len(ids) {
		return 0, fmt.Errorf("%w: entry %q names %d accounts, %d exist",
			ErrUnknownAccount, e.IdempotencyKey, len(ids), len(rows))
	}
	type lockedAccount struct {
		kind     Kind
		currency Currency
		posted   Amount
	}
	accounts := make(map[AccountID]*lockedAccount, len(rows))
	for _, r := range rows {
		k := Kind(r.Kind)
		if !k.Valid() {
			return 0, fmt.Errorf("ledger: account %d has unknown kind %q", r.ID, r.Kind)
		}
		accounts[AccountID(r.ID)] = &lockedAccount{kind: k, currency: Currency(r.Currency), posted: Amount(r.Posted)}
	}

	// 4. Apply each posting on the account's normal side.
	for _, p := range e.Postings {
		a := accounts[p.Account]
		if a.currency != p.Currency {
			return 0, fmt.Errorf("%w: posting in %s to account %d in %s",
				ErrInvalidEntry, p.Currency, p.Account, a.currency)
		}
		if p.Direction == a.kind.NormalSide() {
			a.posted, err = a.posted.Add(p.Amount)
		} else {
			a.posted, err = a.posted.Sub(p.Amount)
		}
		if err != nil {
			return 0, fmt.Errorf("ledger: account %d: %w", p.Account, err)
		}
	}
	for _, id := range ids {
		a := accounts[AccountID(id)]
		if a.kind.IsCustomer() && a.posted < 0 {
			return 0, fmt.Errorf("%w: account %d (%s) would be %s",
				ErrInsufficientFunds, id, a.kind, a.posted)
		}
	}

	// 5. Write postings and balances.
	for _, p := range e.Postings {
		err := q.InsertPosting(ctx, ledgerdb.InsertPostingParams{
			EntryID:   id,
			AccountID: int64(p.Account),
			Currency:  string(p.Currency),
			Direction: string(p.Direction),
			Amount:    int64(p.Amount),
		})
		if err != nil {
			return 0, fmt.Errorf("ledger: insert posting: %w", err)
		}
	}
	for _, id := range ids {
		err := q.SetPosted(ctx, ledgerdb.SetPostedParams{ID: id, Posted: int64(accounts[AccountID(id)].posted)})
		if err != nil {
			return 0, fmt.Errorf("ledger: update balance of account %d: %w", id, err)
		}
	}
	return entryID, nil
}

func mapInsertEntryError(err error, e Entry) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case conReversesUnique:
			return fmt.Errorf("%w: entry %d", ErrAlreadyReversed, *e.Reverses)
		case conReversesFK:
			return fmt.Errorf("%w: reverses unknown entry %d", ErrInvalidEntry, *e.Reverses)
		}
	}
	return fmt.Errorf("ledger: insert entry: %w", err)
}

// OpenHouseAccounts creates Ferry's house accounts for USDC if they do not
// exist yet, and returns their ids. Safe to call repeatedly.
func OpenHouseAccounts(ctx context.Context, db DB) (HouseAccounts, error) {
	q := ledgerdb.New(db)
	ids := make(map[Kind]AccountID, len(HouseKinds))
	for _, k := range HouseKinds {
		p := ledgerdb.InsertHouseAccountParams{Kind: string(k), Currency: string(USDC)}
		if err := q.InsertHouseAccount(ctx, p); err != nil {
			return HouseAccounts{}, fmt.Errorf("ledger: open house account %s: %w", k, err)
		}
		id, err := q.HouseAccountID(ctx, ledgerdb.HouseAccountIDParams(p))
		if err != nil {
			return HouseAccounts{}, fmt.Errorf("ledger: find house account %s: %w", k, err)
		}
		ids[k] = AccountID(id)
	}
	return HouseAccounts{
		ChainOmnibus:  ids[KindChainOmnibus],
		NoahPrefunded: ids[KindNoahPrefunded],
		Fees:          ids[KindFees],
	}, nil
}

// OpenCustomerAccounts creates a customer's available and held accounts
// for USDC if they do not exist yet, and returns their ids. Safe to call
// repeatedly.
func OpenCustomerAccounts(ctx context.Context, db DB, owner uuid.UUID) (CustomerAccounts, error) {
	q := ledgerdb.New(db)
	ids := make(map[Kind]AccountID, 2)
	for _, k := range []Kind{KindAvailable, KindHeld} {
		err := q.InsertCustomerAccount(ctx, ledgerdb.InsertCustomerAccountParams{
			OwnerID: &owner, Kind: string(k), Currency: string(USDC),
		})
		if err != nil {
			return CustomerAccounts{}, fmt.Errorf("ledger: open %s account: %w", k, err)
		}
		id, err := q.CustomerAccountID(ctx, ledgerdb.CustomerAccountIDParams{
			OwnerID: &owner, Kind: string(k), Currency: string(USDC),
		})
		if err != nil {
			return CustomerAccounts{}, fmt.Errorf("ledger: find %s account: %w", k, err)
		}
		ids[k] = AccountID(id)
	}
	return CustomerAccounts{Available: ids[KindAvailable], Held: ids[KindHeld]}, nil
}

// Balance returns an account's posted balance on its normal side.
func Balance(ctx context.Context, db DB, id AccountID) (Amount, error) {
	posted, err := ledgerdb.New(db).AccountBalance(ctx, int64(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %d", ErrUnknownAccount, id)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger: balance of account %d: %w", id, err)
	}
	return Amount(posted), nil
}
