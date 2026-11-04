package ledger

import "errors"

// Sentinel errors. Callers match them with errors.Is; the wrapped message
// carries the details.
var (
	// ErrInvalidAmount: a decimal string is not a valid USDC amount.
	ErrInvalidAmount = errors.New("ledger: invalid amount")
	// ErrOverflow: an amount or balance does not fit in int64.
	ErrOverflow = errors.New("ledger: amount overflow")
	// ErrInvalidEntry: the entry breaks a rule (unbalanced, bad type, ...).
	// Nothing was written.
	ErrInvalidEntry = errors.New("ledger: invalid entry")
	// ErrUnknownAccount: a posting names an account that does not exist.
	ErrUnknownAccount = errors.New("ledger: unknown account")
	// ErrInsufficientFunds: the entry would make a customer account
	// negative. Nothing was written.
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	// ErrDuplicateEntry: an entry with this idempotency key was already
	// posted. The existing entry's id is returned alongside; for a replayed
	// step this means "already done".
	ErrDuplicateEntry = errors.New("ledger: duplicate entry")
	// ErrAlreadyReversed: another correction already mirrors this entry.
	ErrAlreadyReversed = errors.New("ledger: entry already reversed")
)
