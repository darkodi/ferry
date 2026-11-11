package recon_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/destination"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
	"github.com/darkodi/ferry/internal/recon"
	"github.com/darkodi/ferry/internal/treasury"
)

type railEnv struct {
	t     *testing.T
	fake  *noahtest.Server
	rl    *noah.Rail
	svc   *payout.Service
	dests *destination.Store
	tre   *treasury.Service
	cust  customer.Customer
	dest  uuid.UUID
}

func newRailEnv(t *testing.T) *railEnv {
	t.Helper()
	needDB(t)
	fake := noahtest.New("k", nil)
	t.Cleanup(fake.Close)
	client, err := noah.NewClient(fake.BaseURL(), "k", nil)
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
	e := &railEnv{
		t: t, fake: fake, rl: rl, dests: dests,
		svc: payout.NewService(pool, rl, dests, house, time.Now),
		tre: treasury.New(pool, "noah", rl, noahtest.Crypto, house),
	}
	ctx := t.Context()
	if e.cust, _, err = customer.Create(ctx, pool, "Rail recon"); err != nil {
		t.Fatal(err)
	}
	err = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := ledger.PostEntry(ctx, tx, ledger.Deposit("recon-test", uuid.NewString(), 0, uuid.New(), house, e.cust.Accounts, 1000_000000))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := dests.Create(ctx, destination.CreateRequest{
		CustomerID: e.cust.ID, Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch",
		Form: []byte(`{"BankDetails":{"AccountNumber":"12345678","BankCode":"123456789"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.dest = d.ID
	return e
}

// submitted creates, confirms and submits a 100 USD payout (100.5 USDC
// spent at the fake's price once settled).
func (e *railEnv) submitted() payout.Payout {
	e.t.Helper()
	ctx := e.t.Context()
	p, err := e.svc.Create(ctx, payout.CreateRequest{
		CustomerID: e.cust.ID, IdempotencyKey: uuid.NewString(), FiatAmount: 100_00, FiatCurrency: "USD", DestinationID: e.dest,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if p, err = e.svc.Confirm(ctx, e.cust.ID, p.ID); err != nil {
		e.t.Fatal(err)
	}
	if p, err = e.svc.Submit(ctx, p.ID); err != nil || p.Status != payout.Submitted {
		e.t.Fatalf("submit: %v %v", p.Status, err)
	}
	return p
}

func (e *railEnv) tx(p payout.Payout) string {
	e.t.Helper()
	for _, t := range e.fake.Transactions() {
		if t.ExternalID == p.ID.String() {
			return t.ID
		}
	}
	e.t.Fatalf("no Noah transaction for %s", p.ID)
	return ""
}

func (e *railEnv) check() map[string]recon.Break {
	e.t.Helper()
	now := time.Now()
	bs, _, err := recon.NewRail(pool, "noah", e.rl, house).Run(e.t.Context(), recon.Window{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		e.t.Fatal(err)
	}
	m := map[string]recon.Break{}
	for _, b := range bs {
		m[b.Kind] = b
	}
	return m
}

func wantKinds(t *testing.T, got map[string]recon.Break, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("breaks %v, want exactly %v", got, want)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("breaks %v, want %v", got, want)
		}
	}
}

// One scenario through every rail break, then the repairs an operator
// would make. The ledger is shared, so this is one test, step by step.
func TestRailCheck(t *testing.T) {
	e := newRailEnv(t)
	ctx := t.Context()

	// A top-up, booked from Noah's own record; booking it again is a no-op.
	topup := e.fake.Credit("1000")
	if tp, err := e.tre.Record(ctx, topup); err != nil || !tp.New || tp.Amount != 1000_000000 {
		t.Fatalf("top-up: %+v %v", tp, err)
	}
	if tp, err := e.tre.Record(ctx, topup); err != nil || tp.New {
		t.Fatalf("second booking: %+v %v", tp, err)
	}

	// A settled payout, synced: everything agrees.
	first := e.submitted()
	if err := e.fake.Settle(e.tx(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Sync(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, e.check())

	// Now break things.
	// 1. A sale Ferry knows nothing about.
	q, err := e.rl.Prepare(ctx, rail.QuoteRequest{FiatAmount: 5_00, FiatCurrency: "USD", Destination: rail.Destination{
		Country: "US", FiatCurrency: "USD", PaymentMethodType: "BankAch", Form: []byte(`{"BankDetails":{"AccountNumber":"99999999","BankCode":"123456789"}}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.rl.Submit(ctx, q, uuid.NewString(), "not-ferry"); err != nil {
		t.Fatal(err)
	}
	// 2. A top-up nobody booked.
	unbooked := e.fake.Credit("5")
	// 3. A payout Noah settled whose webhook never arrived.
	second := e.submitted()
	if err := e.fake.Settle(e.tx(second)); err != nil {
		t.Fatal(err)
	}
	// 4. A bank return not booked.
	if _, err := e.fake.Reverse(e.tx(first), "100"); err != nil {
		t.Fatal(err)
	}
	// 5. A Ferry rail attempt whose id Noah does not know.
	third := e.submitted()
	if _, err := pool.Exec(ctx, `UPDATE rail_transactions SET rail_tx_id = $1 WHERE payout_id = $2`, uuid.NewString(), third.ID); err != nil {
		t.Fatal(err)
	}

	got := e.check()
	wantKinds(t, got, "unknown_rail_tx", "topup_not_booked", "status_mismatch", "return_not_booked", "missing_at_rail", "balance")
	if b := got["status_mismatch"]; b.Detail["rail"] != "settled" || b.Detail["payout"] != "submitted" {
		t.Fatalf("status_mismatch %+v", b)
	}
	// Noah: 1000 + 5 - 100.5 - 100.5 + 100. Ledger: 1000 - 100.5 so far.
	if b := got["balance"]; b.Detail["rail"] != "904.000000" || b.Detail["ledger"] != "899.500000" {
		t.Fatalf("balance break %+v", b)
	}

	// Repairs: book the top-up, sync the two payouts. What remains is
	// what really needs a person: the foreign sale and the unknown id.
	if _, err := e.tre.Record(ctx, unbooked); err != nil {
		t.Fatal(err)
	}
	for _, p := range []payout.Payout{first, second} {
		if _, err := e.svc.Sync(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	wantKinds(t, e.check(), "unknown_rail_tx", "missing_at_rail")
}

// Only a settled incoming credit that is not a return can be a top-up.
func TestTreasuryRefusesNonTopups(t *testing.T) {
	e := newRailEnv(t)
	ctx := t.Context()
	p := e.submitted()
	if _, err := e.tre.Record(ctx, e.tx(p)); !errors.Is(err, treasury.ErrNotTopup) {
		t.Fatalf("an outgoing sale: %v", err)
	}
	if err := e.fake.Settle(e.tx(p)); err != nil {
		t.Fatal(err)
	}
	ret, err := e.fake.Reverse(e.tx(p), "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.tre.Record(ctx, ret); !errors.Is(err, treasury.ErrNotTopup) {
		t.Fatalf("a bank return: %v", err)
	}
	if _, err := e.tre.Record(ctx, uuid.NewString()); !errors.Is(err, treasury.ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	// Leave the ledger as Noah has it for later tests: book the return.
	if _, err := e.svc.Sync(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}
