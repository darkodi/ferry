// Package mock is an in-memory Rail for tests and local development. It
// behaves like a real rail where it matters to Ferry: submits are
// deduplicated by nonce, a timeout can hide a transaction that was in fact
// created, and settlement, failure and bank returns arrive later as new
// versions of the transaction.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/rail"
)

// Pricing: 1 USDC = 1 USD. A quote authorizes the USDC needed plus a 0.5%
// fee plus a 1% buffer; by default a settlement spends the needed amount
// and charges the fee, so the buffer returns to the customer.
const (
	feeDivisor    = 200 // 0.5%
	bufferDivisor = 100 // 1%
	quoteTTL      = 5 * time.Minute
	microPerCent  = 10_000 // 1 cent = 0.01 USDC = 10,000 millionths
)

// Outcome scripts what Submit does for one payout.
type Outcome int

// Submit outcomes.
const (
	// Default: settle at once when AutoSettle is on, else stay pending.
	Default Outcome = iota
	// Reject: refuse the submit; no transaction is created.
	Reject
	// Timeout: create the transaction but return an error, so the caller
	// cannot tell whether it went through.
	Timeout
)

// Rail is the mock. The zero value is not usable; call New.
type Rail struct {
	// AutoSettle makes every default submit settle immediately. Used by
	// the local dev server so a payout completes without manual steps.
	AutoSettle bool

	mu      sync.Mutex
	now     func() time.Time
	seq     int
	quotes  map[string]quoteInfo // by rail quote id
	txs     map[string]*rail.Tx  // by rail tx id
	byNonce map[string]string    // nonce -> rail tx id
	plans   map[string]Outcome   // external id -> outcome
}

type quoteInfo struct {
	quote  rail.Quote
	needed ledger.Amount
	fee    ledger.Amount
}

var _ rail.Rail = (*Rail)(nil)

// New returns an empty mock rail that reads the time from now.
func New(now func() time.Time) *Rail {
	return &Rail{
		now:     now,
		quotes:  map[string]quoteInfo{},
		txs:     map[string]*rail.Tx{},
		byNonce: map[string]string{},
		plans:   map[string]Outcome{},
	}
}

// Name implements rail.Rail.
func (*Rail) Name() string { return "mock" }

// Plan scripts the Submit outcome for the payout with this external id.
func (m *Rail) Plan(externalID string, o Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[externalID] = o
}

// Prepare implements rail.Rail. Only USD is supported.
func (m *Rail) Prepare(ctx context.Context, req rail.QuoteRequest) (rail.Quote, error) {
	if err := ctx.Err(); err != nil {
		return rail.Quote{}, fmt.Errorf("mock: %w", err)
	}
	if req.FiatCurrency != "USD" {
		return rail.Quote{}, fmt.Errorf("%w: currency %s not supported", rail.ErrRejected, req.FiatCurrency)
	}
	const maxCents = (1 << 62) / microPerCent // keeps all sums below int64 overflow
	if req.FiatAmount <= 0 || req.FiatAmount > maxCents {
		return rail.Quote{}, fmt.Errorf("%w: amount out of range", rail.ErrRejected)
	}
	needed := ledger.Amount(req.FiatAmount * microPerCent)
	fee := needed / feeDivisor
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	q := rail.Quote{
		RailQuoteID:      fmt.Sprintf("mq_%d", m.seq),
		FiatAmount:       req.FiatAmount,
		FiatCurrency:     req.FiatCurrency,
		CryptoAuthorized: needed + fee + needed/bufferDivisor,
		ExpiresAt:        m.now().Add(quoteTTL),
	}
	m.quotes[q.RailQuoteID] = quoteInfo{quote: q, needed: needed, fee: fee}
	return q, nil
}

// Submit implements rail.Rail.
func (m *Rail) Submit(ctx context.Context, q rail.Quote, nonce, externalID string) (rail.Tx, error) {
	if err := ctx.Err(); err != nil {
		return rail.Tx{}, fmt.Errorf("mock: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byNonce[nonce]; ok { // a retry: same transaction, no second payout
		return *m.txs[id], nil
	}
	info, ok := m.quotes[q.RailQuoteID]
	if !ok {
		return rail.Tx{}, fmt.Errorf("%w: unknown quote %s", rail.ErrRejected, q.RailQuoteID)
	}
	plan := m.plans[externalID]
	if plan == Reject {
		return rail.Tx{}, fmt.Errorf("%w: destination refused", rail.ErrRejected)
	}
	m.seq++
	tx := &rail.Tx{
		ID:         fmt.Sprintf("mtx_%d", m.seq),
		ExternalID: externalID,
		Status:     rail.StatusPending,
		SubStatus:  "screening",
		Version:    1,
	}
	if plan == Default && m.AutoSettle {
		tx.Status, tx.SubStatus, tx.Spent, tx.Fee = rail.StatusSettled, "", info.needed, info.fee
	}
	m.txs[tx.ID] = tx
	m.byNonce[nonce] = tx.ID
	if plan == Timeout {
		return rail.Tx{}, fmt.Errorf("mock: submit %s: %w", externalID, context.DeadlineExceeded)
	}
	return *tx, nil
}

// Get implements rail.Rail.
func (m *Rail) Get(ctx context.Context, railTxID string) (rail.Tx, error) {
	if err := ctx.Err(); err != nil {
		return rail.Tx{}, fmt.Errorf("mock: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[railTxID]
	if !ok {
		return rail.Tx{}, fmt.Errorf("%w: %s", rail.ErrNotFound, railTxID)
	}
	out := *tx
	if tx.Return != nil {
		r := *tx.Return
		out.Return = &r
	}
	return out, nil
}

// TxByExternalID returns the transaction created for a payout, if any.
// Tests use it to find a transaction whose submit timed out.
func (m *Rail) TxByExternalID(externalID string) (rail.Tx, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range m.txs {
		if tx.ExternalID == externalID {
			return *tx, true
		}
	}
	return rail.Tx{}, false
}

// SetSubStatus moves a pending transaction to another sub-status, as a
// compliance review would.
func (m *Rail) SetSubStatus(railTxID, sub string) error {
	return m.update(railTxID, rail.StatusPending, func(tx *rail.Tx) { tx.SubStatus = sub })
}

// Settle settles a pending transaction for spent USDC plus fee.
func (m *Rail) Settle(railTxID string, spent, fee ledger.Amount) error {
	return m.update(railTxID, rail.StatusPending, func(tx *rail.Tx) {
		tx.Status, tx.SubStatus, tx.Spent, tx.Fee = rail.StatusSettled, "", spent, fee
	})
}

// Fail fails a pending transaction, as a compliance rejection would.
func (m *Rail) Fail(railTxID string) error {
	return m.update(railTxID, rail.StatusPending, func(tx *rail.Tx) {
		tx.Status, tx.SubStatus = rail.StatusFailed, ""
	})
}

// ReturnFunds records a bank return of a settled transaction.
func (m *Rail) ReturnFunds(railTxID string, amount ledger.Amount) error {
	return m.update(railTxID, rail.StatusSettled, func(tx *rail.Tx) {
		m.seq++
		tx.Return = &rail.Return{RailTxID: fmt.Sprintf("mtx_%d", m.seq), Amount: amount}
	})
}

func (m *Rail) update(railTxID string, want rail.Status, f func(*rail.Tx)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, ok := m.txs[railTxID]
	if !ok {
		return fmt.Errorf("%w: %s", rail.ErrNotFound, railTxID)
	}
	if tx.Status != want {
		return fmt.Errorf("mock: tx %s is %s, want %s", railTxID, tx.Status, want)
	}
	f(tx)
	tx.Version++
	return nil
}
