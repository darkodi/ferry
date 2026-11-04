package ledger

import (
	"context"
	"fmt"

	"github.com/darkodi/ferry/internal/ledger/ledgerdb"
)

// Invariant check names reported in Violation.Check.
const (
	// CheckUnbalanced: all postings in a currency do not sum to zero.
	CheckUnbalanced = "postings_unbalanced"
	// CheckDrift: an account's posted balance differs from its postings.
	CheckDrift = "balance_drift"
	// CheckCustomerNegative: a customer account is below zero.
	CheckCustomerNegative = "customer_negative"
	// CheckHouseNegative: a house account is below zero. This is allowed
	// (house accounts mirror balances Ferry does not control) but alerts:
	// usually a missing treasury top-up.
	CheckHouseNegative = "house_negative"
)

// Violation is one failed invariant.
type Violation struct {
	Check   string
	Account AccountID // 0 when the check is not about one account
	Detail  string
}

// CheckInvariants runs the ledger-only invariants from docs/design.md over
// the whole ledger and returns every violation found. Invariants that need
// payout data (held = sum of open authorized amounts) arrive with M2.
func CheckInvariants(ctx context.Context, db DB) ([]Violation, error) {
	q := ledgerdb.New(db)
	var out []Violation

	unbalanced, err := q.UnbalancedCurrencies(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: invariant %s: %w", CheckUnbalanced, err)
	}
	for _, r := range unbalanced {
		out = append(out, Violation{
			Check:  CheckUnbalanced,
			Detail: fmt.Sprintf("%s postings net to %s", r.Currency, Amount(r.Net)),
		})
	}

	drifted, err := q.DriftedAccounts(ctx, debitNormalKinds())
	if err != nil {
		return nil, fmt.Errorf("ledger: invariant %s: %w", CheckDrift, err)
	}
	for _, r := range drifted {
		out = append(out, Violation{
			Check:   CheckDrift,
			Account: AccountID(r.ID),
			Detail:  fmt.Sprintf("posted %s, postings sum to %s", Amount(r.Posted), Amount(r.FromPostings)),
		})
	}

	negative, err := q.NegativeAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: invariant negative balances: %w", err)
	}
	for _, r := range negative {
		check := CheckHouseNegative
		if Kind(r.Kind).IsCustomer() {
			check = CheckCustomerNegative
		}
		out = append(out, Violation{
			Check:   check,
			Account: AccountID(r.ID),
			Detail:  fmt.Sprintf("%s balance %s", r.Kind, Amount(r.Posted)),
		})
	}
	return out, nil
}
