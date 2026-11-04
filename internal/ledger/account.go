package ledger

// AccountID identifies a row in the accounts table.
type AccountID int64

// Kind is what an account is for. See "Account kinds" in docs/design.md.
type Kind string

// Account kinds. The strings match the accounts.kind CHECK constraint.
const (
	KindAvailable     Kind = "available"      // customer: spendable
	KindHeld          Kind = "held"           // customer: reserved for a payout
	KindChainOmnibus  Kind = "chain_omnibus"  // house: Ferry's USDC on-chain
	KindNoahPrefunded Kind = "noah_prefunded" // house: Ferry's USDC at Noah
	KindFees          Kind = "fees"           // house: Ferry's own fee income
)

// HouseKinds lists the kinds that belong to Ferry rather than a customer.
var HouseKinds = []Kind{KindChainOmnibus, KindNoahPrefunded, KindFees}

// Valid reports whether k is one of the five kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindAvailable, KindHeld, KindChainOmnibus, KindNoahPrefunded, KindFees:
		return true
	}
	return false
}

// IsCustomer reports whether accounts of this kind have an owner and
// must never go negative.
func (k Kind) IsCustomer() bool {
	return k == KindAvailable || k == KindHeld
}

// NormalSide is the direction that increases an account's balance:
// credit for what Ferry owes customers and for revenue, debit for
// Ferry's assets.
func (k Kind) NormalSide() Direction {
	switch k {
	case KindAvailable, KindHeld, KindFees:
		return Credit
	case KindChainOmnibus, KindNoahPrefunded:
		return Debit
	}
	panic("ledger: unknown account kind " + string(k)) // unreachable after Valid()
}

// debitNormalKinds feeds the invariant query, keeping Go the single
// source of truth for each kind's normal side.
func debitNormalKinds() []string {
	var out []string
	for _, k := range []Kind{KindAvailable, KindHeld, KindChainOmnibus, KindNoahPrefunded, KindFees} {
		if k.NormalSide() == Debit {
			out = append(out, string(k))
		}
	}
	return out
}

// Currency is a ledger currency code. v1 supports only USDC.
type Currency string

// USDC is the only ledger currency in v1.
const USDC Currency = "USDC"

// Direction says which side of an account a posting is on.
type Direction string

// Posting directions. The strings match the postings.direction CHECK.
const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// CustomerAccounts are one customer's two accounts in one currency.
type CustomerAccounts struct {
	Available AccountID
	Held      AccountID
}

// HouseAccounts are Ferry's own accounts in one currency.
type HouseAccounts struct {
	ChainOmnibus  AccountID
	NoahPrefunded AccountID
	Fees          AccountID
}
