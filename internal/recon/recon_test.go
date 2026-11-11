package recon_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/recon"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool  *pgxpool.Pool
	house ledger.HouseAccounts
)

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
	return m.Run()
}

var quiet = slog.New(slog.DiscardHandler)

func needDB(t *testing.T) {
	t.Helper()
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
}

// scripted is a checker whose answers the test sets.
type scripted struct {
	mu     sync.Mutex
	check  string
	scope  string
	breaks []recon.Break
	err    error
}

func (s *scripted) Check() string { return s.check }
func (s *scripted) Scope() string { return s.scope }
func (s *scripted) Run(context.Context, recon.Window) ([]recon.Break, []recon.Match, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.breaks, nil, s.err
}
func (s *scripted) set(err error, bs ...recon.Break) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breaks, s.err = bs, err
}

var window = recon.Window{From: mustTime("2026-11-10T00:00:00Z"), To: mustTime("2026-11-11T00:00:00Z")}

func runOnce(t *testing.T, r *recon.Recon) recon.Report {
	t.Helper()
	rep, err := r.Run(t.Context(), window)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

type breakRow struct {
	seen     int
	cleared  bool
	resolved bool
}

func breakState(t *testing.T, kind, subject string) []breakRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT seen_count, cleared_at IS NOT NULL, resolved_at IS NOT NULL
		FROM recon_breaks WHERE kind = $1 AND subject = $2 ORDER BY first_seen`, kind, subject)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (breakRow, error) {
		var b breakRow
		return b, r.Scan(&b.seen, &b.cleared, &b.resolved)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func liveBreakID(t *testing.T, kind, subject string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM recon_breaks WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL`,
		kind, subject).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A break lives as one row while it lasts, stays resolved while its
// detail is unchanged, reopens when the detail changes, and is cleared
// when a run no longer sees it.
func TestBreakLifecycle(t *testing.T) {
	needDB(t)
	c := &scripted{check: "chain", scope: "lifecycle:"}
	r := recon.New(pool, quiet, c)
	b := recon.Break{Check: "chain", Kind: "address_balance", Subject: "lifecycle:0xA", Detail: map[string]string{"on_chain": "5"}}

	c.set(nil, b)
	if rep := runOnce(t, r); rep.New != 1 || rep.Breaks != 1 {
		t.Fatalf("first run %+v", rep)
	}
	runOnce(t, r)
	if s := breakState(t, b.Kind, b.Subject); len(s) != 1 || s[0].seen != 2 || s[0].cleared {
		t.Fatalf("after a second sighting: %+v", s)
	}

	if err := recon.Resolve(t.Context(), pool, liveBreakID(t, b.Kind, b.Subject), "known: test deposit before start"); err != nil {
		t.Fatal(err)
	}
	runOnce(t, r) // same detail: stays resolved
	if s := breakState(t, b.Kind, b.Subject); !s[0].resolved {
		t.Fatalf("a resolved break reopened with an unchanged detail: %+v", s)
	}

	b.Detail = map[string]string{"on_chain": "7"}
	c.set(nil, b)
	runOnce(t, r) // detail changed: reopened
	if s := breakState(t, b.Kind, b.Subject); s[0].resolved {
		t.Fatalf("a changed break stayed resolved: %+v", s)
	}

	c.set(nil)
	if rep := runOnce(t, r); rep.Cleared != 1 {
		t.Fatalf("cleared %d, want 1", rep.Cleared)
	}
	if s := breakState(t, b.Kind, b.Subject); !s[0].cleared {
		t.Fatalf("not cleared: %+v", s)
	}

	c.set(nil, b) // it comes back: a new break, the old one stays cleared
	runOnce(t, r)
	if s := breakState(t, b.Kind, b.Subject); len(s) != 2 || !s[0].cleared || s[1].cleared {
		t.Fatalf("recurrence: %+v", s)
	}
}

// A checker that cannot run (its source is down) leaves its breaks
// alone; another checker of the same kind clears only its own.
func TestFailedCheckKeepsItsBreaks(t *testing.T) {
	needDB(t)
	evm := &scripted{check: "chain", scope: "iso-evm:"}
	sol := &scripted{check: "chain", scope: "iso-sol:"}
	r := recon.New(pool, quiet, evm, sol)
	be := recon.Break{Check: "chain", Kind: "address_balance", Subject: "iso-evm:0xA", Detail: map[string]string{"x": "1"}}
	bs := recon.Break{Check: "chain", Kind: "address_balance", Subject: "iso-sol:Abc", Detail: map[string]string{"x": "1"}}
	evm.set(nil, be)
	sol.set(nil, bs)
	runOnce(t, r)

	sol.set(errors.New("rpc down"))
	evm.set(nil) // evm's break went away
	rep := runOnce(t, r)
	if len(rep.Failed) != 1 || rep.Failed[0] != "chain:iso-sol:" {
		t.Fatalf("failed %v", rep.Failed)
	}
	if s := breakState(t, bs.Kind, bs.Subject); s[0].cleared {
		t.Fatal("the failed checker's break was cleared")
	}
	if s := breakState(t, be.Kind, be.Subject); !s[0].cleared {
		t.Fatal("the completed checker's gone break was not cleared")
	}
}

// A checker may not write breaks outside its scope.
func TestBreakOutsideScopeRefused(t *testing.T) {
	needDB(t)
	c := &scripted{check: "chain", scope: "mine:"}
	c.set(nil, recon.Break{Check: "chain", Kind: "k", Subject: "theirs:x", Detail: map[string]string{}})
	if _, err := recon.New(pool, quiet, c).Run(t.Context(), window); err == nil {
		t.Fatal("a break outside the checker's scope was stored")
	}
}

func TestResolveNeedsReasonAndOpenBreak(t *testing.T) {
	needDB(t)
	if err := recon.Resolve(t.Context(), pool, uuid.New(), "a good reason"); !errors.Is(err, recon.ErrBreakNotOpen) {
		t.Fatalf("unknown break: %v", err)
	}
	for _, bad := range []string{"", "  ", "ok", "bad\x00reason"} {
		if err := recon.Resolve(t.Context(), pool, uuid.New(), bad); !errors.Is(err, recon.ErrBadReason) {
			t.Fatalf("reason %q: %v", bad, err)
		}
	}
}

// ---- internal ----

// A payout held without its hold entry is a held_mismatch break.
func TestInternalCheck(t *testing.T) {
	needDB(t)
	ctx := t.Context()
	c, _, err := customer.Create(ctx, pool, "Internal")
	if err != nil {
		t.Fatal(err)
	}
	pid := uuid.New()
	// As the owner role: a state the application could never produce.
	if _, err := pool.Exec(ctx, `INSERT INTO payouts (id, customer_id, idempotency_key, fiat_amount, fiat_currency, destination, status)
		VALUES ($1, $2, 'k', 100, 'USD', 'x', 'held')`, pid, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO quotes (id, payout_id, rail, rail_quote_id, fiat_amount, fiat_currency, crypto_authorized, expires_at)
		VALUES (gen_random_uuid(), $1, 'mock', 'q', 100, 'USD', 1015000, now())`, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE payouts SET status = 'failed' WHERE id = $1`, pid)
	})

	bs, _, err := recon.NewInternal(pool).Run(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Kind == "held_mismatch" && b.Subject == c.ID.String() {
			return
		}
	}
	t.Fatalf("no held_mismatch for %s in %+v", c.ID, bs)
}

// ---- chain ----

type fakeBalances struct {
	chain string
	point uint64
	bal   map[string]ledger.Amount
}

func (f *fakeBalances) Chain() string                                 { return f.chain }
func (f *fakeBalances) Point(context.Context, uint64) (uint64, error) { return f.point, nil }
func (f *fakeBalances) Balance(_ context.Context, a string, _ uint64) (ledger.Amount, error) {
	return f.bal[a], nil
}

// seedChain makes a customer with an address on chain and records
// deposits (status, block, amount) to it; confirmed ones are also posted
// to the ledger, as the watcher would.
func seedChain(t *testing.T, chain, addr string, deps ...[3]any) {
	t.Helper()
	ctx := t.Context()
	c, _, err := customer.Create(ctx, pool, "Chain "+chain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO deposit_addresses (customer_id, chain, address, derivation_index) VALUES ($1, $2, $3, 0)`, c.ID, chain, addr); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chain_cursors (chain, last_block, last_hash) VALUES ($1, 100, 'h')`, chain); err != nil {
		t.Fatal(err)
	}
	for i, d := range deps {
		status, block, amount := d[0].(string), d[1].(int), d[2].(int64)
		id := uuid.New()
		tx := fmt.Sprintf("0x%s%d", chain, i)
		if _, err := pool.Exec(ctx, `INSERT INTO deposits (id, chain, tx_hash, log_index, block_number, block_hash, address, customer_id, amount, status)
			VALUES ($1, $2, $3, 0, $4, 'h', $5, $6, $7, $8)`, id, chain, tx, block, addr, c.ID, amount, status); err != nil {
			t.Fatal(err)
		}
		if status == "confirmed" {
			err := db.WithTx(ctx, pool, pgx.TxOptions{}, func(dbtx pgx.Tx) error {
				_, err := ledger.PostEntry(ctx, dbtx, ledger.Deposit(chain, tx, 0, id, house, c.Accounts, ledger.Amount(amount)))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func kinds(bs []recon.Break) map[string]recon.Break {
	m := map[string]recon.Break{}
	for _, b := range bs {
		m[b.Kind+" "+b.Subject] = b
	}
	return m
}

func TestChainCheck(t *testing.T) {
	needDB(t)
	// 5 confirmed at block 10, 3 pending at block 20, 9 pending at block 90
	// (beyond the reading point 50, so not expected on-chain there).
	seedChain(t, "recon-a", "0xA", [3]any{"confirmed", 10, int64(5)}, [3]any{"pending", 20, int64(3)}, [3]any{"pending", 90, int64(9)})
	r := &fakeBalances{chain: "recon-a", point: 50, bal: map[string]ledger.Amount{"0xA": 8}}
	bs, _, err := recon.NewChain(pool, r).Run(t.Context(), window)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 0 {
		t.Fatalf("a consistent chain has breaks: %+v", bs)
	}

	r.bal["0xA"] = 18 // 10 more on-chain than recorded: a missed transfer
	bs, _, _ = recon.NewChain(pool, r).Run(t.Context(), window)
	b, ok := kinds(bs)["address_balance recon-a:0xA"]
	if !ok || b.Detail["on_chain"] != "0.000018" || b.Detail["recorded"] != "0.000008" {
		t.Fatalf("breaks %+v", bs)
	}
}

// A confirmed deposit the ledger never credited is a ledger_deposits
// break; Solana-style readers (point = now) count every recorded deposit.
func TestChainLedgerCheck(t *testing.T) {
	needDB(t)
	seedChain(t, "recon-b", "Sol1", [3]any{"confirmed", 10, int64(5)})
	if _, err := pool.Exec(t.Context(), `INSERT INTO deposits (id, chain, tx_hash, log_index, block_number, block_hash, address, customer_id, amount, status)
		SELECT gen_random_uuid(), chain, 'unposted', 0, 11, 'h', address, customer_id, 7, 'confirmed' FROM deposit_addresses WHERE chain = 'recon-b'`); err != nil {
		t.Fatal(err)
	}
	r := &fakeBalances{chain: "recon-b", point: math.MaxInt64, bal: map[string]ledger.Amount{"Sol1": 12}}
	bs, _, err := recon.NewChain(pool, r).Run(t.Context(), window)
	if err != nil {
		t.Fatal(err)
	}
	m := kinds(bs)
	if len(bs) != 1 {
		t.Fatalf("breaks %+v, want only ledger_deposits", bs)
	}
	if b, ok := m["ledger_deposits recon-b:ledger"]; !ok || b.Detail["confirmed"] != "0.000012" || b.Detail["ledger"] != "0.000005" {
		t.Fatalf("breaks %+v", bs)
	}
}

// A chain the watcher never scanned has nothing to compare yet.
func TestChainNotStarted(t *testing.T) {
	needDB(t)
	bs, _, err := recon.NewChain(pool, &fakeBalances{chain: "recon-none"}).Run(t.Context(), window)
	if err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
