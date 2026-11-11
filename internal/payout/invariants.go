package payout

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout/payoutdb"
)

// Payout invariant names, reported in Violation.Check.
const (
	// CheckHeldMismatch: a customer's held balance differs from the
	// authorized amounts of their payouts in held or submitted.
	CheckHeldMismatch = "held_mismatch"
	// CheckSettleEntries: a settled or reversed payout without exactly
	// one settle entry.
	CheckSettleEntries = "settle_entries"
)

// Violation is one broken payout invariant.
type Violation struct {
	Check  string
	ID     uuid.UUID // customer or payout, depending on the check
	Detail string
}

// CheckInvariants checks the payout invariants from docs/design.md:
//   - a customer's held balance equals the authorized amounts of their
//     payouts in held or submitted (so terminal payouts hold nothing);
//   - every settled or reversed payout has exactly one settle entry.
//
// The ledger's own invariants are checked by ledger.CheckInvariants.
func CheckInvariants(ctx context.Context, d ledger.DB) ([]Violation, error) {
	q := payoutdb.New(d)
	var out []Violation
	held, err := q.HeldMismatches(ctx)
	if err != nil {
		return nil, fmt.Errorf("payout: held check: %w", err)
	}
	for _, r := range held {
		out = append(out, Violation{Check: CheckHeldMismatch, ID: r.CustomerID, Detail: fmt.Sprintf(
			"held balance %s, open payouts authorize %s", ledger.Amount(r.Held), ledger.Amount(r.Expected))})
	}
	settles, err := q.SettleEntryCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("payout: settle check: %w", err)
	}
	for _, r := range settles {
		out = append(out, Violation{Check: CheckSettleEntries, ID: r.ID, Detail: fmt.Sprintf("%d settle entries, want 1", r.Entries)})
	}
	return out, nil
}
