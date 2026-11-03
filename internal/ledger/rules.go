package ledger

import (
	"fmt"

	"github.com/google/uuid"
)

// Builders for the seven entry types in docs/design.md ("Entry types").
// Each one fixes the accounts, directions and idempotency key, so callers
// only supply ids and amounts. Builders do not validate amounts; PostEntry
// calls Validate before writing anything.

func line(acc AccountID, dir Direction, amt Amount) Posting {
	return Posting{Account: acc, Currency: USDC, Direction: dir, Amount: amt}
}

// DepositKey is the idempotency key of the deposit entry for one on-chain
// transfer, identified by chain, transaction hash and log index.
func DepositKey(chain, txHash string, logIndex uint64) string {
	return fmt.Sprintf("deposit:%s:%s:%d", chain, txHash, logIndex)
}

// Deposit credits a confirmed on-chain deposit to the customer.
// Debit chain_omnibus, credit available.
func Deposit(chain, txHash string, logIndex uint64, depositID uuid.UUID, h HouseAccounts, c CustomerAccounts, amount Amount) Entry {
	return Entry{
		IdempotencyKey: DepositKey(chain, txHash, logIndex),
		Type:           TypeDeposit,
		RefID:          &depositID,
		Postings: []Posting{
			line(h.ChainOmnibus, Debit, amount),
			line(c.Available, Credit, amount),
		},
	}
}

// TreasuryTopup records USDC moved from the omnibus wallet to Ferry's
// Noah balance. Debit noah_prefunded, credit chain_omnibus. ref is a
// unique reference for the move, such as its on-chain transaction hash.
func TreasuryTopup(ref string, h HouseAccounts, amount Amount) Entry {
	return Entry{
		IdempotencyKey: "topup:" + ref,
		Type:           TypeTreasuryTopup,
		Postings: []Posting{
			line(h.NoahPrefunded, Debit, amount),
			line(h.ChainOmnibus, Credit, amount),
		},
	}
}

// Hold reserves the rail's authorized amount for a payout.
// Debit available, credit held.
func Hold(payoutID uuid.UUID, c CustomerAccounts, authorized Amount) Entry {
	return Entry{
		IdempotencyKey: payoutKey(payoutID, "hold"),
		Type:           TypeHold,
		RefID:          &payoutID,
		Postings: []Posting{
			line(c.Available, Debit, authorized),
			line(c.Held, Credit, authorized),
		},
	}
}

// Settle books a settled payout. The authorized amount splits as
// authorized = actual + railFee + remainder: actual and railFee leave
// through noah_prefunded, the remainder returns to available (and is
// omitted when zero). It fails if actual + railFee exceeds authorized.
func Settle(payoutID uuid.UUID, h HouseAccounts, c CustomerAccounts, authorized, actual, railFee Amount) (Entry, error) {
	spent, err := actual.Add(railFee)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: settle: %w", ErrInvalidEntry, err)
	}
	remainder, err := authorized.Sub(spent)
	if err != nil {
		return Entry{}, fmt.Errorf("%w: settle: %w", ErrInvalidEntry, err)
	}
	if remainder < 0 {
		return Entry{}, fmt.Errorf("%w: settle: actual %s + fee %s exceeds authorized %s",
			ErrInvalidEntry, actual, railFee, authorized)
	}
	postings := []Posting{
		line(c.Held, Debit, authorized),
		line(h.NoahPrefunded, Credit, spent),
	}
	if remainder > 0 {
		postings = append(postings, line(c.Available, Credit, remainder))
	}
	return Entry{
		IdempotencyKey: payoutKey(payoutID, "settle"),
		Type:           TypeSettle,
		RefID:          &payoutID,
		Postings:       postings,
	}, nil
}

// Release returns a failed payout's hold to the customer. A payout has a
// single release key, so it can be released at most once.
// Debit held, credit available.
func Release(payoutID uuid.UUID, c CustomerAccounts, amount Amount) Entry {
	return Entry{
		IdempotencyKey: payoutKey(payoutID, "release"),
		Type:           TypeRelease,
		RefID:          &payoutID,
		Postings: []Posting{
			line(c.Held, Debit, amount),
			line(c.Available, Credit, amount),
		},
	}
}

// BankReturn credits the amount the bank returned for a settled payout;
// the customer bears any shortfall. railTxID is the rail's id for the
// return transaction. Debit noah_prefunded, credit available.
func BankReturn(payoutID uuid.UUID, railTxID string, h HouseAccounts, c CustomerAccounts, returned Amount) Entry {
	return Entry{
		IdempotencyKey: payoutKey(payoutID, "return:"+railTxID),
		Type:           TypeBankReturn,
		RefID:          &payoutID,
		Postings: []Posting{
			line(h.NoahPrefunded, Debit, returned),
			line(c.Available, Credit, returned),
		},
	}
}

// Correction is a human-approved fix with arbitrary balanced postings.
// ref is a unique reference, such as a recon break id.
func Correction(ref string, postings []Posting) Entry {
	return Entry{
		IdempotencyKey: "correction:" + ref,
		Type:           TypeCorrection,
		Postings:       postings,
	}
}

// Mirror is a correction that exactly undoes one earlier entry: the same
// postings with directions swapped, linked through Reverses. The database
// allows at most one mirror per entry.
func Mirror(ref string, original EntryID, originalPostings []Posting) Entry {
	postings := make([]Posting, len(originalPostings))
	for i, p := range originalPostings {
		p.Direction = opposite(p.Direction)
		postings[i] = p
	}
	e := Correction(ref, postings)
	e.Reverses = &original
	return e
}

func opposite(d Direction) Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

func payoutKey(payoutID uuid.UUID, step string) string {
	return "payout:" + payoutID.String() + ":" + step
}
