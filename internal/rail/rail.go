// Package rail defines the Rail interface that Ferry's payout core talks to.
// Implementations live in subpackages (mock/ now, noah/ in M5) and keep all
// rail-specific details to themselves.
package rail

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/ledger"
)

// Rail is a fiat off-ramp: it prices a payout, executes it against Ferry's
// pre-funded balance, and reports what happened.
//
// Error contract for Submit: ErrRejected means the rail definitely did not
// create a transaction, so the hold may be released. Any other error means
// the outcome is unknown; the caller must not release and must retry with
// the same nonce.
type Rail interface {
	// Name identifies the rail in stored rows, e.g. "mock" or "noah".
	Name() string
	// Prepare prices a payout. It moves no money.
	Prepare(ctx context.Context, req QuoteRequest) (Quote, error)
	// Submit executes a quoted payout. nonce is unique per attempt and
	// reused on every retry of that attempt; externalID is the payout id.
	Submit(ctx context.Context, q Quote, nonce, externalID string) (Tx, error)
	// Get fetches the rail's current view of a transaction.
	Get(ctx context.Context, railTxID string) (Tx, error)
}

// QuoteRequest asks the rail to price a payout of a fiat amount to a bank
// account.
type QuoteRequest struct {
	PayoutID     uuid.UUID
	FiatAmount   int64 // minor units, e.g. cents
	FiatCurrency string
	Destination  Destination
}

// Destination is the bank account a payout goes to, as the customer
// registered it. Form holds the bank details in the rail's own shape;
// it is sensitive and must never be logged.
type Destination struct {
	ID                uuid.UUID
	Country           string // ISO 3166-1 alpha-2
	FiatCurrency      string
	PaymentMethodType string // e.g. BankAch
	Form              json.RawMessage
	// PaymentMethodID is the rail's saved payment method for this
	// account, once a payout has settled to it; empty before.
	PaymentMethodID string
}

// Quote is the rail's price.
type Quote struct {
	RailQuoteID  string
	FiatAmount   int64
	FiatCurrency string
	// CryptoAuthorized is the most USDC the rail may take, fees and price
	// movement included. Ferry holds exactly this much.
	CryptoAuthorized ledger.Amount
	ExpiresAt        time.Time
}

// Status is a rail transaction's status.
type Status string

// Rail transaction statuses, matching rail_transactions.status.
const (
	StatusPending Status = "pending"
	StatusSettled Status = "settled"
	StatusFailed  Status = "failed"
)

// Tx is the rail's view of one payout transaction.
type Tx struct {
	ID         string
	ExternalID string
	Status     Status
	SubStatus  string // e.g. "screening", "review"; informational
	// Version increases with every change; older events are ignored.
	Version int64
	// Spent and Fee are set when Status is StatusSettled: the USDC the
	// rail used for the fiat amount, and its fee. Spent + Fee never
	// exceeds the quote's CryptoAuthorized.
	Spent ledger.Amount
	Fee   ledger.Amount
	// Return is set when the bank sent a settled payout back.
	Return *Return
	// PaymentMethodID is the rail's saved payment method the payout went
	// to, if the rail reports one; Ferry reuses it for later payouts.
	PaymentMethodID string
}

// Return is a bank return of a settled payout, reported by the rail as a
// new transaction that points at the original.
type Return struct {
	RailTxID string
	Amount   ledger.Amount
}

// Event is a verified webhook from the rail. It only says "look again":
// Ferry stores it and re-fetches the transaction before moving money.
type Event struct {
	Rail     string // e.g. "noah"
	Type     string // the rail's event type, e.g. "Transaction"
	RailTxID string // the transaction the event is about
	// Reverses is set when this transaction reverses (returns) an earlier
	// one; the payout is found through that earlier transaction.
	Reverses string
	Version  int64 // increases with every change of the transaction
	Occurred time.Time
	Payload  []byte // the verified body, kept for audit
}

// Errors.
var (
	// ErrRejected: the rail refused the request and created nothing.
	ErrRejected = errors.New("rail: rejected")
	// ErrNotFound: the rail has no such transaction.
	ErrNotFound = errors.New("rail: not found")
	// ErrBadSignature: a webhook was not signed by the rail. Store
	// nothing and answer 401.
	ErrBadSignature = errors.New("rail: webhook signature does not verify")
	// ErrIgnoredEvent: a verified webhook Ferry has no use for. Answer
	// 2xx so the rail stops retrying.
	ErrIgnoredEvent = errors.New("rail: webhook event not used")
)
