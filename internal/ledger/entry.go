package ledger

import (
	"fmt"

	"github.com/google/uuid"
)

// EntryID identifies a row in the journal_entries table.
type EntryID int64

// EntryType says which business event an entry records. See "Entry types"
// in docs/design.md.
type EntryType string

// Entry types. The strings match the journal_entries.type CHECK.
const (
	TypeDeposit       EntryType = "deposit"
	TypeTreasuryTopup EntryType = "treasury_topup"
	TypeHold          EntryType = "hold"
	TypeSettle        EntryType = "settle"
	TypeRelease       EntryType = "release"
	TypeBankReturn    EntryType = "bank_return"
	TypeCorrection    EntryType = "correction"
)

// Valid reports whether t is one of the seven entry types.
func (t EntryType) Valid() bool {
	switch t {
	case TypeDeposit, TypeTreasuryTopup, TypeHold, TypeSettle, TypeRelease, TypeBankReturn, TypeCorrection:
		return true
	}
	return false
}

// maxKeyLen matches the journal_entries idempotency_key CHECK.
const maxKeyLen = 200

// Posting is one line of an entry.
type Posting struct {
	Account   AccountID
	Currency  Currency
	Direction Direction
	Amount    Amount // always > 0; Direction carries the sign
}

// Entry is one business event and its postings. Build entries with the
// functions in rules.go rather than by hand.
type Entry struct {
	IdempotencyKey string
	Type           EntryType
	RefID          *uuid.UUID // the deposit or payout this entry belongs to, if any
	Reverses       *EntryID   // set only by a correction that mirrors one entry
	Postings       []Posting
}

// Validate checks the rules that need no database: a known type, a key,
// at least two postings with positive amounts, and debits equal to
// credits per currency. PostEntry calls it before writing anything.
func (e Entry) Validate() error {
	if n := len(e.IdempotencyKey); n == 0 || n > maxKeyLen {
		return fmt.Errorf("%w: idempotency key length %d not in 1..%d", ErrInvalidEntry, n, maxKeyLen)
	}
	if !e.Type.Valid() {
		return fmt.Errorf("%w: unknown type %q", ErrInvalidEntry, e.Type)
	}
	if e.Reverses != nil && e.Type != TypeCorrection {
		return fmt.Errorf("%w: only a correction may reverse an entry", ErrInvalidEntry)
	}
	if len(e.Postings) < 2 {
		return fmt.Errorf("%w: %d posting(s), at least 2 required", ErrInvalidEntry, len(e.Postings))
	}

	// Net per currency: debits add, credits subtract. Must end at zero.
	net := map[Currency]Amount{}
	for i, p := range e.Postings {
		if p.Account <= 0 {
			return fmt.Errorf("%w: posting %d has no account", ErrInvalidEntry, i)
		}
		if p.Currency != USDC {
			return fmt.Errorf("%w: posting %d has unsupported currency %q", ErrInvalidEntry, i, p.Currency)
		}
		if p.Amount <= 0 {
			return fmt.Errorf("%w: posting %d amount %s is not positive", ErrInvalidEntry, i, p.Amount)
		}
		var err error
		switch p.Direction {
		case Debit:
			net[p.Currency], err = net[p.Currency].Add(p.Amount)
		case Credit:
			net[p.Currency], err = net[p.Currency].Sub(p.Amount)
		default:
			return fmt.Errorf("%w: posting %d has direction %q", ErrInvalidEntry, i, p.Direction)
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidEntry, err)
		}
	}
	for cur, n := range net {
		if n != 0 {
			return fmt.Errorf("%w: unbalanced in %s by %s", ErrInvalidEntry, cur, n)
		}
	}
	return nil
}
