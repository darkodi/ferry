package payout_test

// End-to-end payout tests through the real Noah adapter, against the fake
// Noah server (signatures required) and a real Postgres. They cover the
// paths the Noah sandbox cannot trigger on demand: failures, lost answers,
// reviews and bad settlement reports. Skipped with -short.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
)

type noahEnv struct {
	t     *testing.T
	fake  *noahtest.Server
	svc   *payout.Service
	dests *destination.Store
}

func newNoahEnv(t *testing.T) *noahEnv {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fake := noahtest.New("k", &key.PublicKey)
	t.Cleanup(fake.Close)
	signer, err := noah.NewSigner(key, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := noah.NewClient(fake.BaseURL(), "k", signer)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := noah.New(client, noah.Config{CryptoCurrency: noahtest.Crypto}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	dests, err := destination.NewStore(pool, "noah", make([]byte, destination.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return &noahEnv{t: t, fake: fake, svc: payout.NewService(pool, rl, dests, house, time.Now), dests: dests}
}

// destination registers a BankAch account with Noah's rail for c.
func (e *noahEnv) destination(c customer.Customer) uuid.UUID {
	e.t.Helper()
	d, err := e.dests.Create(context.Background(), destination.CreateRequest{
		CustomerID: c.ID, Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch",
		Form: []byte(`{"BankDetails":{"AccountNumber":"12345678","BankCode":"123456789"},"PaymentPurpose":"Invoice"}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return d.ID
}

// held creates and confirms a 100 USD payout to dest: 101.5 USDC held.
func (e *noahEnv) held(c customer.Customer, dest uuid.UUID) payout.Payout {
	e.t.Helper()
	ctx := context.Background()
	p, err := e.svc.Create(ctx, payout.CreateRequest{
		CustomerID: c.ID, IdempotencyKey: uuid.NewString(), FiatAmount: 100_00, FiatCurrency: "USD", DestinationID: dest,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	wantStatus(e.t, p, payout.Quoted)
	if p, err = e.svc.Confirm(ctx, c.ID, p.ID); err != nil {
		e.t.Fatal(err)
	}
	wantStatus(e.t, p, payout.Held)
	return p
}

func (e *noahEnv) submit(p payout.Payout) payout.Payout {
	e.t.Helper()
	p, err := e.svc.Submit(context.Background(), p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *noahEnv) sync(p payout.Payout) payout.Payout {
	e.t.Helper()
	p, err := e.svc.Sync(context.Background(), p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *noahEnv) noahTx(p payout.Payout) string {
	e.t.Helper()
	for _, tx := range e.fake.Transactions() {
		if tx.ExternalID == p.ID.String() {
			return tx.ID
		}
	}
	e.t.Fatalf("no Noah transaction for payout %s", p.ID)
	return ""
}

func subStatus(t *testing.T, p payout.Payout) string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(),
		`SELECT sub_status FROM rail_transactions WHERE payout_id = $1 ORDER BY attempt DESC LIMIT 1`, p.ID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

// The happy path: quote, hold, sell, review, settle. The settle releases
// the unused buffer, and Noah's saved payment method is remembered so the
// next payout to the same account sends no bank details.
func TestNoahPayoutSettles(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "300")
	dest := e.destination(c)

	p := e.submit(e.held(c, dest))
	wantStatus(t, p, payout.Submitted)
	wantBalances(t, c, "198.5", "101.5")
	tx := e.noahTx(p)

	// A review at the same progress: recorded, nothing moves.
	if err := e.fake.SetSubStatus(tx, "UnderReview", "AwaitingCustomer"); err != nil {
		t.Fatal(err)
	}
	p = e.sync(p)
	wantStatus(t, p, payout.Submitted)
	if got := subStatus(t, p); got != "UnderReview, RFI AwaitingCustomer" {
		t.Fatalf("sub-status %q", got)
	}

	if err := e.fake.Settle(tx); err != nil {
		t.Fatal(err)
	}
	p = e.sync(p)
	wantStatus(t, p, payout.Settled)
	// 300 - 100 to the bank - 0.50 fee; the 1.00 buffer came back.
	wantBalances(t, c, "199.5", "0")
	p = e.sync(p) // a repeated report changes nothing
	wantStatus(t, p, payout.Settled)

	d, err := e.dests.Get(context.Background(), c.ID, dest)
	if err != nil || d.RailPaymentMethodID == "" {
		t.Fatalf("payment method not remembered: %+v, %v", d, err)
	}
	second := e.submit(e.held(c, dest))
	wantStatus(t, second, payout.Submitted)
}

// Noah refuses the sell (Ferry's Noah balance is too low): the payout
// fails and the customer's hold is released in full.
func TestNoahInsufficientBalanceReleases(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200")
	p := e.held(c, e.destination(c))
	e.fake.FailNextSell(noahtest.Fault{Status: 402, Type: "InsufficientBalance"})
	p = e.submit(p)
	wantStatus(t, p, payout.Failed)
	wantBalances(t, c, "200", "0")
	if n := len(e.fake.Transactions()); n != 0 {
		t.Fatalf("%d Noah transactions, want 0", n)
	}
}

// Failure table: "Worker dies after rail call" / "Rail call times out".
// Noah created the sale but answered 500: the payout stays held with
// needs_resolution, and the resend reuses the nonce, so Noah still has
// exactly one transaction.
func TestNoahLostAnswerResentWithSameNonce(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200")
	p := e.held(c, e.destination(c))
	e.fake.FailNextSell(noahtest.Fault{Status: 500, Type: "Unexpected", Create: true})
	p = e.submit(p)
	wantStatus(t, p, payout.Held)
	if !p.NeedsResolution {
		t.Fatal("unknown outcome not marked needs_resolution")
	}
	wantBalances(t, c, "98.5", "101.5")

	p = e.submit(p) // what the resolver does
	wantStatus(t, p, payout.Submitted)
	if n := len(e.fake.Transactions()); n != 1 || railAttempts(t, p) != 1 {
		t.Fatalf("%d Noah transactions, %d attempts; want 1 and 1", n, railAttempts(t, p))
	}
}

// Failure table: "Submit 4xx after an unknown outcome". The sale exists
// at Noah but the answer was a refusal: the adapter finds it by
// ExternalID, so the hold is not released for money that is moving.
func TestNoahRefusedButCreatedIsNotReleased(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200")
	p := e.held(c, e.destination(c))
	e.fake.FailNextSell(noahtest.Fault{Status: 400, Type: "InvalidMessage", Create: true})
	p = e.submit(p)
	wantStatus(t, p, payout.Submitted)
	wantBalances(t, c, "98.5", "101.5")
}

// A compliance rejection after review: Noah fails the sale (and refunds
// Ferry's balance); the customer's hold is released.
func TestNoahFailedAfterReviewReleases(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200")
	p := e.submit(e.held(c, e.destination(c)))
	if err := e.fake.Fail(e.noahTx(p)); err != nil {
		t.Fatal(err)
	}
	p = e.sync(p)
	wantStatus(t, p, payout.Failed)
	wantBalances(t, c, "200", "0")
}

// Failure table: "Breakdown missing at settle". Noah says Settled but its
// amounts do not add up: nothing is posted, the payout stays submitted
// and Sync reports the error for a human.
func TestNoahUnexplainedSettlementNotApplied(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200")
	p := e.submit(e.held(c, e.destination(c)))
	if err := e.fake.SettleWithBreakdown(e.noahTx(p), "100.5", map[string]string{"Remaining": "100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Sync(context.Background(), p.ID); err == nil {
		t.Fatal("an unexplained settlement was applied")
	}
	p = load(t, p)
	wantStatus(t, p, payout.Submitted)
	wantBalances(t, c, "98.5", "101.5")
}

// A destination registered for the mock rail cannot be paid through Noah:
// its form was never checked against Noah's channels.
func TestNoahRefusesOtherRailsDestination(t *testing.T) {
	e := newNoahEnv(t)
	c := newCustomer(t, "200") // also registers a mock-rail destination
	_, err := e.svc.Create(context.Background(), payout.CreateRequest{
		CustomerID: c.ID, IdempotencyKey: uuid.NewString(), FiatAmount: 100_00, FiatCurrency: "USD", DestinationID: destOf(c),
	})
	if !errors.Is(err, destination.ErrInvalid) {
		t.Fatalf("got %v, want destination.ErrInvalid", err)
	}
}
