package ledger_test

// Database tests for PostEntry and the invariant checker, on a throwaway
// Postgres 16 (testcontainers). Skipped with -short; unit tests in this
// package still run.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool  *pgxpool.Pool
	house ledger.HouseAccounts
)

// usdc converts whole USDC to an Amount, for readable test amounts.
func usdc(s string) ledger.Amount {
	a, err := ledger.ParseUSDC(s)
	if err != nil {
		panic(err)
	}
	return a
}

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run()) // database tests skip themselves
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

	cfg, err := pgxpool.ParseConfig(tdb.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg.MaxConns = 20 // enough for the concurrency tests to contend
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()

	if house, err = ledger.OpenHouseAccounts(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// ---- helpers ----

func needDB(t *testing.T) {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
}

func newCustomer(t *testing.T) ledger.CustomerAccounts {
	t.Helper()
	c, err := ledger.OpenCustomerAccounts(context.Background(), pool, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// post runs PostEntry in its own READ COMMITTED transaction (with retry).
func post(t *testing.T, e ledger.Entry) (ledger.EntryID, error) {
	t.Helper()
	return postWith(pgx.TxOptions{}, e)
}

func postWith(opts pgx.TxOptions, e ledger.Entry) (ledger.EntryID, error) {
	var id ledger.EntryID
	err := db.WithTx(context.Background(), pool, opts, func(tx pgx.Tx) error {
		var err error
		id, err = ledger.PostEntry(context.Background(), tx, e)
		return err
	})
	return id, err
}

func mustPost(t *testing.T, e ledger.Entry) ledger.EntryID {
	t.Helper()
	id, err := post(t, e)
	if err != nil {
		t.Fatalf("post %s: %v", e.IdempotencyKey, err)
	}
	return id
}

func deposit(t *testing.T, c ledger.CustomerAccounts, amount ledger.Amount) {
	t.Helper()
	mustPost(t, ledger.Deposit("evm-test", uuid.NewString(), 0, uuid.New(), house, c, amount))
}

func balance(t *testing.T, id ledger.AccountID) ledger.Amount {
	t.Helper()
	b, err := ledger.Balance(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireBalance(t *testing.T, name string, id ledger.AccountID, want ledger.Amount) {
	t.Helper()
	if got := balance(t, id); got != want {
		t.Errorf("%s balance = %s, want %s", name, got, want)
	}
}

func entryExists(t *testing.T, key string) bool {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM journal_entries WHERE idempotency_key = $1`, key).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// requireInvariants fails on any violation except house_negative, which is
// an alert rather than a broken invariant.
func requireInvariants(t *testing.T) {
	t.Helper()
	vs, err := ledger.CheckInvariants(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Check != ledger.CheckHouseNegative {
			t.Errorf("invariant violated: %+v", v)
		}
	}
}

// ---- happy paths ----

// TestWorkedExample replays the worked example from docs/design.md.
func TestWorkedExample(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	omnibus0, noah0 := balance(t, house.ChainOmnibus), balance(t, house.NoahPrefunded)
	payout := uuid.New()

	deposit(t, c, usdc("100"))
	mustPost(t, ledger.Hold(payout, c, usdc("50.20")))
	requireBalance(t, "available after hold", c.Available, usdc("49.80"))
	requireBalance(t, "held after hold", c.Held, usdc("50.20"))

	mustPost(t, ledger.TreasuryTopup(uuid.NewString(), house, usdc("60")))
	settle, err := ledger.Settle(payout, house, c, usdc("50.20"), usdc("49.50"), usdc("0.30"))
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, settle)

	requireBalance(t, "available", c.Available, usdc("50.20"))
	requireBalance(t, "held", c.Held, 0)
	requireBalance(t, "chain_omnibus delta", house.ChainOmnibus, omnibus0+usdc("40"))
	requireBalance(t, "noah_prefunded delta", house.NoahPrefunded, noah0+usdc("10.20"))
	requireInvariants(t)
}

func TestReleaseAndBankReturn(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	deposit(t, c, usdc("100"))

	failed := uuid.New()
	mustPost(t, ledger.Hold(failed, c, usdc("30")))
	mustPost(t, ledger.Release(failed, c, usdc("30")))
	requireBalance(t, "available after release", c.Available, usdc("100"))

	returned := uuid.New()
	mustPost(t, ledger.Hold(returned, c, usdc("40")))
	settle, err := ledger.Settle(returned, house, c, usdc("40"), usdc("40"), 0)
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, settle)
	mustPost(t, ledger.BankReturn(returned, "rtx-1", house, c, usdc("39.10"))) // customer bears 0.90
	requireBalance(t, "available after return", c.Available, usdc("99.10"))
	requireInvariants(t)
}

func TestOpenAccountsIsIdempotent(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	owner := uuid.New()
	a, err := ledger.OpenCustomerAccounts(ctx, pool, owner)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ledger.OpenCustomerAccounts(ctx, pool, owner)
	if err != nil || a != b {
		t.Fatalf("reopen customer: %+v vs %+v, %v", a, b, err)
	}
	h, err := ledger.OpenHouseAccounts(ctx, pool)
	if err != nil || h != house {
		t.Fatalf("reopen house: %+v vs %+v, %v", h, house, err)
	}
}

// ---- refusals: nothing is written ----

func TestDuplicateKeyReturnsExistingEntry(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	e := ledger.Deposit("evm-test", "0xdup", 1, uuid.New(), house, c, usdc("5"))
	first := mustPost(t, e)
	second, err := post(t, e)
	if !errors.Is(err, ledger.ErrDuplicateEntry) || second != first {
		t.Fatalf("second post = %d, %v; want %d, ErrDuplicateEntry", second, err, first)
	}
	requireBalance(t, "available", c.Available, usdc("5"))
}

func TestReleaseAtMostOnce(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	deposit(t, c, usdc("10"))
	p := uuid.New()
	mustPost(t, ledger.Hold(p, c, usdc("10")))
	mustPost(t, ledger.Release(p, c, usdc("10")))
	if _, err := post(t, ledger.Release(p, c, usdc("10"))); !errors.Is(err, ledger.ErrDuplicateEntry) {
		t.Fatalf("second release: want ErrDuplicateEntry, got %v", err)
	}
	requireBalance(t, "available", c.Available, usdc("10"))
}

func TestInsufficientFundsWritesNothing(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	deposit(t, c, usdc("1"))
	e := ledger.Hold(uuid.New(), c, usdc("1.000001"))
	if _, err := post(t, e); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if entryExists(t, e.IdempotencyKey) {
		t.Fatal("refused entry was written")
	}
	requireBalance(t, "available", c.Available, usdc("1"))
	requireBalance(t, "held", c.Held, 0)
}

func TestInvalidEntryWritesNothing(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	e := ledger.Correction(uuid.NewString(), []ledger.Posting{
		{Account: c.Available, Currency: ledger.USDC, Direction: ledger.Credit, Amount: 2},
		{Account: house.ChainOmnibus, Currency: ledger.USDC, Direction: ledger.Debit, Amount: 1},
	})
	if _, err := post(t, e); !errors.Is(err, ledger.ErrInvalidEntry) {
		t.Fatalf("want ErrInvalidEntry, got %v", err)
	}
	if entryExists(t, e.IdempotencyKey) {
		t.Fatal("invalid entry was written")
	}
}

func TestUnknownAccount(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	e := ledger.Hold(uuid.New(), ledger.CustomerAccounts{Available: c.Available, Held: 1 << 40}, 1)
	if _, err := post(t, e); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("want ErrUnknownAccount, got %v", err)
	}
	if _, err := ledger.Balance(context.Background(), pool, 1<<40); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("Balance: want ErrUnknownAccount, got %v", err)
	}
}

func TestMirrorCorrection(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	e := ledger.Deposit("evm-test", uuid.NewString(), 0, uuid.New(), house, c, usdc("7"))
	original := mustPost(t, e)

	mustPost(t, ledger.Mirror(uuid.NewString(), original, e.Postings))
	requireBalance(t, "available after mirror", c.Available, 0)

	if _, err := post(t, ledger.Mirror(uuid.NewString(), original, e.Postings)); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second mirror: want ErrAlreadyReversed, got %v", err)
	}
	if _, err := post(t, ledger.Mirror(uuid.NewString(), 1<<40, e.Postings)); !errors.Is(err, ledger.ErrInvalidEntry) {
		t.Fatalf("mirror of unknown entry: want ErrInvalidEntry, got %v", err)
	}
	requireInvariants(t)
}

func TestHouseNegativeIsReported(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	// Settle more than Ferry holds at Noah, without a top-up.
	amount := balance(t, house.NoahPrefunded) + usdc("1")
	if amount < usdc("1") {
		amount = usdc("1")
	}
	p := uuid.New()
	deposit(t, c, amount)
	mustPost(t, ledger.Hold(p, c, amount))
	settle, err := ledger.Settle(p, house, c, amount, amount, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, settle) // house accounts may go negative: still posts

	vs, err := ledger.CheckInvariants(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vs {
		found = found || (v.Check == ledger.CheckHouseNegative && v.Account == house.NoahPrefunded)
	}
	if !found {
		t.Fatalf("want a house_negative alert for noah_prefunded, got %+v", vs)
	}
	// Restore a non-negative Noah balance for later tests.
	mustPost(t, ledger.TreasuryTopup(uuid.NewString(), house, amount))
	requireInvariants(t)
}

// TestInvariantsDetectDrift corrupts a balance directly and expects the
// checker to notice.
func TestInvariantsDetectDrift(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	deposit(t, c, usdc("3"))
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // never commit the corruption
	if _, err := tx.Exec(ctx, `UPDATE accounts SET posted = posted + 1 WHERE id = $1`, c.Available); err != nil {
		t.Fatal(err)
	}
	vs, err := ledger.CheckInvariants(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || vs[0].Check != ledger.CheckDrift || vs[0].Account != c.Available {
		t.Fatalf("want balance_drift on %d, got %+v", c.Available, vs)
	}
}

// TestAppRoleCanPostEntry proves the ferry_app grants are enough for
// PostEntry, including the FOR UPDATE row locks.
func TestAppRoleCanPostEntry(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	deposit(t, c, usdc("2"))
	err := db.WithTx(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL ROLE ferry_app`); err != nil {
			return err
		}
		_, err := ledger.PostEntry(context.Background(), tx, ledger.Hold(uuid.New(), c, usdc("2")))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	requireBalance(t, "held", c.Held, usdc("2"))
}

// ---- concurrency ----

// warmPool opens every pool connection up front. Without it the pool dials
// lazily, the first transactions finish while others wait for a
// connection, and the race never really happens.
func warmPool() {
	ctx := context.Background()
	conns := make([]*pgxpool.Conn, 0, pool.Config().MaxConns)
	for range pool.Config().MaxConns {
		c, err := pool.Acquire(ctx)
		if err != nil {
			break
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

// race runs n copies of fn at once and returns the errors.
func race(n int, fn func(i int) error) []error {
	warmPool()
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximise contention
			errs[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// countErrs tallies errors: nil, each sentinel in kinds, and anything else.
func countErrs(errs []error, kinds ...error) (ok int, byKind map[error]int, other []error) {
	byKind = map[error]int{}
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		matched := false
		for _, k := range kinds {
			if errors.Is(err, k) {
				byKind[k]++
				matched = true
				break
			}
		}
		if !matched {
			other = append(other, err)
		}
	}
	return ok, byKind, other
}

func testConcurrentOverspend(t *testing.T, opts pgx.TxOptions) {
	t.Helper()
	c := newCustomer(t)
	deposit(t, c, usdc("100"))

	errs := race(100, func(int) error {
		_, err := postWith(opts, ledger.Hold(uuid.New(), c, usdc("100")))
		return err
	})
	ok, byKind, other := countErrs(errs, ledger.ErrInsufficientFunds)
	if ok != 1 || byKind[ledger.ErrInsufficientFunds] != 99 || len(other) != 0 {
		t.Fatalf("ok=%d insufficient=%d other=%v; want 1, 99, none", ok, byKind[ledger.ErrInsufficientFunds], other)
	}
	requireBalance(t, "available", c.Available, 0)
	requireBalance(t, "held", c.Held, usdc("100"))
	requireInvariants(t)
}

// TestConcurrentOverspend: 100 goroutines try to hold the whole balance.
// Exactly one succeeds; the balance never goes negative.
func TestConcurrentOverspend(t *testing.T) {
	needDB(t)
	testConcurrentOverspend(t, pgx.TxOptions{})
}

// TestConcurrentOverspendSerializable repeats the overspend race under
// SERIALIZABLE, where Postgres aborts conflicting transactions and WithTx
// retries them.
func TestConcurrentOverspendSerializable(t *testing.T) {
	needDB(t)
	testConcurrentOverspend(t, pgx.TxOptions{IsoLevel: pgx.Serializable})
}

// TestConcurrentSameKey: 100 goroutines post the same deposit. One entry
// is written; the others get ErrDuplicateEntry.
func TestConcurrentSameKey(t *testing.T) {
	needDB(t)
	c := newCustomer(t)
	e := ledger.Deposit("evm-test", uuid.NewString(), 0, uuid.New(), house, c, usdc("100"))

	errs := race(100, func(int) error {
		_, err := post(t, e)
		return err
	})
	ok, byKind, other := countErrs(errs, ledger.ErrDuplicateEntry)
	if ok != 1 || byKind[ledger.ErrDuplicateEntry] != 99 || len(other) != 0 {
		t.Fatalf("ok=%d duplicate=%d other=%v; want 1, 99, none", ok, byKind[ledger.ErrDuplicateEntry], other)
	}
	requireBalance(t, "available", c.Available, usdc("100"))
	requireInvariants(t)
}

// TestNoDeadlockOppositeOrder: entries list the same two accounts in
// opposite orders. PostEntry locks by ascending id, so no transaction may
// deadlock. Runs without WithTx so a deadlock surfaces as an error
// instead of being retried away.
func TestNoDeadlockOppositeOrder(t *testing.T) {
	needDB(t)
	a, b := newCustomer(t), newCustomer(t)
	deposit(t, a, usdc("1000"))
	deposit(t, b, usdc("1000"))
	move := func(from, to ledger.AccountID) []ledger.Posting {
		return []ledger.Posting{
			{Account: from, Currency: ledger.USDC, Direction: ledger.Debit, Amount: 1},
			{Account: to, Currency: ledger.USDC, Direction: ledger.Credit, Amount: 1},
		}
	}

	errs := race(100, func(i int) error {
		postings := move(a.Available, b.Available) // lists a first
		if i%2 == 1 {
			postings = move(b.Available, a.Available) // lists b first
		}
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := ledger.PostEntry(ctx, tx, ledger.Correction(uuid.NewString(), postings)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	for _, err := range errs {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("deadlock: %v", err)
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	requireBalance(t, "a", a.Available, usdc("1000"))
	requireBalance(t, "b", b.Available, usdc("1000"))
	requireInvariants(t)
}
