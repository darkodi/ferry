package payout_test

// Service tests on a throwaway Postgres with the mock rail: one test per
// path through the state machine. Each ends by checking the ledger and
// payout invariants. Skipped with -short.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/rail/mock"
	"github.com/darkodi/ferry/internal/testutil/pgtest"

	"github.com/jackc/pgx/v5"
)

var (
	pool  *pgxpool.Pool
	house ledger.HouseAccounts
	rl    *mock.Rail
	svc   *payout.Service
	clk   = &clock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
)

// clock is a test clock that only moves when told to.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer tdb.Close()
	if pool, err = pgxpool.New(ctx, tdb.DSN); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	if house, err = ledger.OpenHouseAccounts(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rl = mock.New(clk.Now)
	svc = payout.NewService(pool, rl, house, clk.Now)
	return m.Run()
}

// ---- helpers ----

func usdc(s string) ledger.Amount {
	a, err := ledger.ParseUSDC(s)
	if err != nil {
		panic(err)
	}
	return a
}

// newCustomer creates a customer funded with the given USDC.
func newCustomer(t *testing.T, funds string) customer.Customer {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	ctx := context.Background()
	c, _, err := customer.Create(ctx, pool, "Acme "+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	e := ledger.Deposit("evm-test", uuid.NewString(), 0, uuid.New(), house, c.Accounts, usdc(funds))
	err = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := ledger.PostEntry(ctx, tx, e)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { checkInvariants(t) })
	return c
}

// create starts a USD payout of the given cents.
func create(t *testing.T, c customer.Customer, cents int64) payout.Payout {
	t.Helper()
	p, err := svc.Create(context.Background(), payout.CreateRequest{
		CustomerID: c.ID, IdempotencyKey: uuid.NewString(),
		FiatAmount: cents, FiatCurrency: "USD", Destination: "bank_acct_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// submitted creates, confirms and submits a 100 USD payout. The mock
// authorizes 101.50 USDC for it (100 + 0.5% fee + 1% buffer).
func submitted(t *testing.T, c customer.Customer) payout.Payout {
	t.Helper()
	ctx := context.Background()
	p := create(t, c, 100_00)
	p, err := svc.Confirm(ctx, c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = svc.Submit(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Submitted)
	return p
}

func railTxID(t *testing.T, p payout.Payout) string {
	t.Helper()
	tx, ok := rl.TxByExternalID(p.ID.String())
	if !ok {
		t.Fatalf("no rail tx for payout %s", p.ID)
	}
	return tx.ID
}

func sync1(t *testing.T, p payout.Payout) payout.Payout {
	t.Helper()
	p, err := svc.Sync(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantStatus(t *testing.T, p payout.Payout, want payout.State) {
	t.Helper()
	if p.Status != want {
		t.Fatalf("status = %s, want %s (reason %q)", p.Status, want, p.FailureReason)
	}
}

func wantBalances(t *testing.T, c customer.Customer, available, held string) {
	t.Helper()
	for _, chk := range []struct {
		name string
		id   ledger.AccountID
		want ledger.Amount
	}{{"available", c.Accounts.Available, usdc(available)}, {"held", c.Accounts.Held, usdc(held)}} {
		got, err := ledger.Balance(context.Background(), pool, chk.id)
		if err != nil {
			t.Fatal(err)
		}
		if got != chk.want {
			t.Errorf("%s = %s, want %s", chk.name, got, chk.want)
		}
	}
}

func checkInvariants(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	lv, err := ledger.CheckInvariants(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range lv {
		// noah_prefunded goes negative here: tests settle payouts without
		// topping it up first. That is an alert, not a ledger error.
		if v.Check != ledger.CheckHouseNegative {
			t.Errorf("ledger invariant: %+v", v)
		}
	}
	pv, err := payout.CheckInvariants(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range pv {
		t.Errorf("payout invariant: %+v", v)
	}
}

// ---- tests ----

func TestSettleReturnsUnusedHold(t *testing.T) {
	c := newCustomer(t, "200")
	p := create(t, c, 100_00)
	wantStatus(t, p, payout.Quoted)
	if p.Quote.CryptoAuthorized != usdc("101.5") {
		t.Fatalf("authorized = %s, want 101.500000", p.Quote.CryptoAuthorized)
	}

	p, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Held)
	wantBalances(t, c, "98.5", "101.5")

	p, err = svc.Submit(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Submitted)
	wantBalances(t, c, "98.5", "101.5") // submitting posts nothing

	if err := rl.Settle(railTxID(t, p), usdc("100"), usdc("0.5")); err != nil {
		t.Fatal(err)
	}
	p = sync1(t, p)
	wantStatus(t, p, payout.Settled)
	wantBalances(t, c, "99.5", "0") // the 1.00 buffer came back

	// A repeated sync (duplicate webhook) changes nothing.
	p = sync1(t, p)
	wantStatus(t, p, payout.Settled)
	wantBalances(t, c, "99.5", "0")
}

func TestReplayedCreateReturnsSamePayout(t *testing.T) {
	c := newCustomer(t, "10")
	req := payout.CreateRequest{
		CustomerID: c.ID, IdempotencyKey: "order-42",
		FiatAmount: 5_00, FiatCurrency: "USD", Destination: "bank_acct_1",
	}
	first, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Quote.ID != first.Quote.ID {
		t.Fatalf("replay made a new payout or quote: %v / %v", first.ID, again.ID)
	}

	req.FiatAmount = 6_00
	if _, err := svc.Create(context.Background(), req); !errors.Is(err, payout.ErrKeyReused) {
		t.Fatalf("different request under the same key: got %v, want ErrKeyReused", err)
	}
}

func TestQuoteRefused(t *testing.T) {
	c := newCustomer(t, "10")
	p, err := svc.Create(context.Background(), payout.CreateRequest{
		CustomerID: c.ID, IdempotencyKey: uuid.NewString(),
		FiatAmount: 5_00, FiatCurrency: "EUR", Destination: "bank_acct_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Failed)
	wantBalances(t, c, "10", "0")
}

func TestInsufficientFunds(t *testing.T) {
	c := newCustomer(t, "50")
	p := create(t, c, 100_00)
	_, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	p, _ = svc.Get(context.Background(), c.ID, p.ID)
	wantStatus(t, p, payout.Quoted) // nothing changed; the customer may top up and retry
	wantBalances(t, c, "50", "0")
}

func TestQuoteExpired(t *testing.T) {
	c := newCustomer(t, "200")
	p := create(t, c, 100_00)
	clk.Add(6 * time.Minute) // mock quotes last 5 minutes
	p, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if !errors.Is(err, payout.ErrQuoteExpired) {
		t.Fatalf("got %v, want ErrQuoteExpired", err)
	}
	wantStatus(t, p, payout.Expired)
	wantBalances(t, c, "200", "0")
}

func TestRailRejectsSubmit(t *testing.T) {
	c := newCustomer(t, "200")
	p := create(t, c, 100_00)
	rl.Plan(p.ID.String(), mock.Reject)
	p, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = svc.Submit(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Failed)
	wantBalances(t, c, "200", "0") // hold released
}

func TestComplianceFailsAfterReview(t *testing.T) {
	c := newCustomer(t, "200")
	p := submitted(t, c)
	id := railTxID(t, p)

	if err := rl.SetSubStatus(id, "review"); err != nil {
		t.Fatal(err)
	}
	p = sync1(t, p)
	wantStatus(t, p, payout.Submitted) // a long review is not a failure
	wantBalances(t, c, "98.5", "101.5")

	if err := rl.Fail(id); err != nil {
		t.Fatal(err)
	}
	p = sync1(t, p)
	wantStatus(t, p, payout.Failed)
	wantBalances(t, c, "200", "0")
}

func TestPartialBankReturn(t *testing.T) {
	c := newCustomer(t, "200")
	p := submitted(t, c)
	id := railTxID(t, p)
	if err := rl.Settle(id, usdc("100"), usdc("0.5")); err != nil {
		t.Fatal(err)
	}
	p = sync1(t, p)
	wantStatus(t, p, payout.Settled)

	// The bank sends back 90 of the 100; the customer bears the shortfall.
	if err := rl.ReturnFunds(id, usdc("90")); err != nil {
		t.Fatal(err)
	}
	p = sync1(t, p)
	wantStatus(t, p, payout.Reversed)
	wantBalances(t, c, "189.5", "0") // 99.5 after settle + 90 returned
}

func TestSubmitTimeoutKeepsHoldAndReusesNonce(t *testing.T) {
	c := newCustomer(t, "200")
	p := create(t, c, 100_00)
	rl.Plan(p.ID.String(), mock.Timeout)
	p, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The rail created the transaction but the answer was lost.
	p, err = svc.Submit(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Held)
	if !p.NeedsResolution {
		t.Fatal("NeedsResolution not set after an unknown outcome")
	}
	wantBalances(t, c, "98.5", "101.5") // not released: the rail may have it

	// Retrying sends the same nonce, so the rail returns the same
	// transaction instead of creating a second payout.
	p, err = svc.Submit(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Submitted)
	if p.NeedsResolution {
		t.Fatal("NeedsResolution still set after the retry succeeded")
	}
	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rail_transactions WHERE payout_id = $1`, p.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("%d rail attempts, want 1", attempts)
	}
}

func TestOtherCustomerCannotSeePayout(t *testing.T) {
	a := newCustomer(t, "10")
	b := newCustomer(t, "10")
	p := create(t, a, 1_00)
	if _, err := svc.Get(context.Background(), b.ID, p.ID); !errors.Is(err, payout.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, err := svc.Confirm(context.Background(), b.ID, p.ID); !errors.Is(err, payout.ErrNotFound) {
		t.Fatalf("confirm by another customer: got %v, want ErrNotFound", err)
	}
}
