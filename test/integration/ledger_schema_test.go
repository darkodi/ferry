// Integration tests for the ledger schema (migrations/00001, 00002).
//
// Each test tries to break one rule from the Ledger section of
// docs/design.md and asserts that Postgres itself refuses, with the exact
// SQLSTATE. The Go ledger code (M1, next PR) is a first line of defence;
// these constraints are the backstop when that code has a bug.
package integration

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

// SQLSTATE codes asserted below.
const (
	codeOK          = ""
	codeCheck       = "23514" // check_violation (CHECK constraints, balance trigger)
	codeUnique      = "23505" // unique_violation
	codeForeignKey  = "23503" // foreign_key_violation
	codeNoPrivilege = "42501" // insufficient_privilege (revoked grants, append-only triggers)
	maxInt64        = "9223372036854775807"
	maxInt64PlusOne = "9223372036854775808"
)

var (
	pool  *pgxpool.Pool
	dsn   string
	house = map[string]int64{} // house account kind -> id
	seq   atomic.Int64
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("skipping integration tests in -short mode")
		os.Exit(0)
	}
	os.Exit(run(m))
}

// run exists so deferred cleanups execute before os.Exit.
func run(m *testing.M) int {
	ctx := context.Background()
	db, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		return 1
	}
	defer db.Close()
	dsn = db.DSN

	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()

	for _, kind := range []string{"chain_omnibus", "noah_prefunded", "fees"} {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO accounts (kind, currency) VALUES ($1, 'USDC') RETURNING id`, kind).Scan(&id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create house account:", err)
			return 1
		}
		house[kind] = id
	}
	return m.Run()
}

// ---- helpers ----

// key returns a unique idempotency key for the calling test.
func key(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s/%d", t.Name(), seq.Add(1))
}

// inTx runs fn in a transaction as the database owner and commits.
// It returns fn's error, or the COMMIT error (where deferred triggers fire).
func inTx(t *testing.T, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return runTx(t, false, fn)
}

// asApp is inTx with the session switched to the least-privilege ferry_app role.
func asApp(t *testing.T, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return runTx(t, true, fn)
}

func runTx(t *testing.T, app bool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if app {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE ferry_app`); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("set role: %v", err)
		}
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// requireCode fails the test unless err carries the given SQLSTATE
// (codeOK means err must be nil).
func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == codeOK {
		if err != nil {
			t.Fatalf("want success, got %v", err)
		}
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want SQLSTATE %s, got %v", want, err)
	}
	if pgErr.Code != want {
		t.Fatalf("want SQLSTATE %s, got %s: %s", want, pgErr.Code, pgErr.Message)
	}
}

// newCustomer opens available and held accounts for a fresh owner.
func newCustomer(t *testing.T) (available, held int64) {
	t.Helper()
	ctx := context.Background()
	err := pool.QueryRow(ctx, `
		WITH o AS (SELECT gen_random_uuid() AS id),
		     a AS (INSERT INTO accounts (owner_id, kind, currency) SELECT id, 'available', 'USDC' FROM o RETURNING id),
		     h AS (INSERT INTO accounts (owner_id, kind, currency) SELECT id, 'held', 'USDC' FROM o RETURNING id)
		SELECT a.id, h.id FROM a, h`).Scan(&available, &held)
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return available, held
}

func insertEntry(tx pgx.Tx, key, typ string) (int64, error) {
	var id int64
	err := tx.QueryRow(context.Background(),
		`INSERT INTO journal_entries (idempotency_key, type) VALUES ($1, $2) RETURNING id`,
		key, typ).Scan(&id)
	return id, err
}

// insertPosting takes amount as a decimal string so tests can go past int64.
func insertPosting(tx pgx.Tx, entryID, accountID int64, direction, amount string) error {
	_, err := tx.Exec(context.Background(),
		`INSERT INTO postings (entry_id, account_id, currency, direction, amount)
		 VALUES ($1, $2, 'USDC', $3, CAST($4::text AS numeric))`,
		entryID, accountID, direction, amount)
	return err
}

// postDeposit commits a balanced deposit entry and returns its id.
func postDeposit(t *testing.T, available int64, amount string) int64 {
	t.Helper()
	var id int64
	err := inTx(t, func(tx pgx.Tx) error {
		var err error
		if id, err = insertEntry(tx, key(t), "deposit"); err != nil {
			return err
		}
		if err := insertPosting(tx, id, house["chain_omnibus"], "debit", amount); err != nil {
			return err
		}
		return insertPosting(tx, id, available, "credit", amount)
	})
	requireCode(t, err, codeOK)
	return id
}

// ---- migrations ----

func TestMigrationsUpDownUp(t *testing.T) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE DATABASE migtest`); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/migtest"

	p, closeDB, err := pgtest.Provider(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// ---- balanced entries (deferred trigger) ----

func TestBalancedEntryCommits(t *testing.T) {
	available, _ := newCustomer(t)
	postDeposit(t, available, "100000000")
}

func TestBalancedEntryWithThreePostingsCommits(t *testing.T) {
	available, held := newCustomer(t)
	// Settle shape: one debit, two credits.
	err := inTx(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "settle")
		if err != nil {
			return err
		}
		if err := insertPosting(tx, id, held, "debit", "50200000"); err != nil {
			return err
		}
		if err := insertPosting(tx, id, house["noah_prefunded"], "credit", "49800000"); err != nil {
			return err
		}
		return insertPosting(tx, id, available, "credit", "400000")
	})
	requireCode(t, err, codeOK)
}

func TestUnbalancedEntryRejectedAtCommit(t *testing.T) {
	available, _ := newCustomer(t)
	err := inTx(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "deposit")
		if err != nil {
			return err
		}
		if err := insertPosting(tx, id, house["chain_omnibus"], "debit", "100"); err != nil {
			return err
		}
		return insertPosting(tx, id, available, "credit", "99")
	})
	requireCode(t, err, codeCheck)
}

func TestEntryWithoutPostingsRejected(t *testing.T) {
	err := inTx(t, func(tx pgx.Tx) error {
		_, err := insertEntry(tx, key(t), "deposit")
		return err
	})
	requireCode(t, err, codeCheck)
}

func TestEntryWithOnePostingRejected(t *testing.T) {
	available, _ := newCustomer(t)
	err := inTx(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "deposit")
		if err != nil {
			return err
		}
		return insertPosting(tx, id, available, "credit", "100")
	})
	requireCode(t, err, codeCheck)
}

func TestPostingAddedLaterToCommittedEntryRejected(t *testing.T) {
	available, _ := newCustomer(t)
	entryID := postDeposit(t, available, "100")
	err := inTx(t, func(tx pgx.Tx) error {
		return insertPosting(tx, entryID, available, "credit", "1")
	})
	requireCode(t, err, codeCheck)
}

// ---- accounts ----

func TestCustomerAccountCannotGoNegative(t *testing.T) {
	available, held := newCustomer(t)
	for _, id := range []int64{available, held} {
		_, err := pool.Exec(context.Background(), `UPDATE accounts SET posted = -1 WHERE id = $1`, id)
		requireCode(t, err, codeCheck)
	}
}

func TestHouseAccountMayGoNegative(t *testing.T) {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Roll back: only the UPDATE being accepted is under test, and the real
	// balance must stay equal to its postings.
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE accounts SET posted = -49800000 WHERE id = $1`, house["noah_prefunded"])
	requireCode(t, err, codeOK)
}

func TestAccountOwnerMustMatchKind(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"customer kind without owner", `INSERT INTO accounts (kind, currency) VALUES ('available', 'USDC')`},
		{"house kind with owner", `INSERT INTO accounts (owner_id, kind, currency) VALUES (gen_random_uuid(), 'fees', 'USDC')`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), tc.sql)
			requireCode(t, err, codeCheck)
		})
	}
}

func TestAccountKindAndCurrencyValid(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"unknown kind", `INSERT INTO accounts (owner_id, kind, currency) VALUES (gen_random_uuid(), 'in_transit', 'USDC')`},
		{"removed kind", `INSERT INTO accounts (kind, currency) VALUES ('return_fees', 'USDC')`},
		{"non-USDC currency", `INSERT INTO accounts (owner_id, kind, currency) VALUES (gen_random_uuid(), 'available', 'USD')`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), tc.sql)
			requireCode(t, err, codeCheck)
		})
	}
}

func TestAccountUniqueness(t *testing.T) {
	ctx := context.Background()
	available, _ := newCustomer(t)

	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (owner_id, kind, currency)
		SELECT owner_id, 'available', 'USDC' FROM accounts WHERE id = $1`, available)
	requireCode(t, err, codeUnique)

	_, err = pool.Exec(ctx, `INSERT INTO accounts (kind, currency) VALUES ('chain_omnibus', 'USDC')`)
	requireCode(t, err, codeUnique)
}

// ---- postings ----

func TestPostingAmountBounds(t *testing.T) {
	available, _ := newCustomer(t)
	tests := []struct {
		amount string
		want   string
	}{
		{"0", codeCheck},
		{"-1", codeCheck},
		{maxInt64PlusOne, codeCheck},
		{maxInt64, codeOK},
	}
	for _, tc := range tests {
		t.Run(tc.amount, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // only the INSERT's immediate CHECK is under test
			id, err := insertEntry(tx, key(t), "deposit")
			if err != nil {
				t.Fatal(err)
			}
			requireCode(t, insertPosting(tx, id, available, "credit", tc.amount), tc.want)
		})
	}
}

func TestPostingDirectionValid(t *testing.T) {
	available, _ := newCustomer(t)
	err := inTx(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "deposit")
		if err != nil {
			return err
		}
		return insertPosting(tx, id, available, "sideways", "1")
	})
	requireCode(t, err, codeCheck)
}

func TestPostingCurrencyMustMatchAccount(t *testing.T) {
	available, _ := newCustomer(t)
	err := inTx(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "deposit")
		if err != nil {
			return err
		}
		_, err = tx.Exec(context.Background(),
			`INSERT INTO postings (entry_id, account_id, currency, direction, amount)
			 VALUES ($1, $2, 'USD', 'credit', 1)`, id, available)
		return err
	})
	requireCode(t, err, codeForeignKey)
}

// ---- journal entries ----

func TestIdempotencyKeyUnique(t *testing.T) {
	available, _ := newCustomer(t)
	k := key(t)
	post := func() error {
		return inTx(t, func(tx pgx.Tx) error {
			id, err := insertEntry(tx, k, "deposit")
			if err != nil {
				return err
			}
			if err := insertPosting(tx, id, house["chain_omnibus"], "debit", "1"); err != nil {
				return err
			}
			return insertPosting(tx, id, available, "credit", "1")
		})
	}
	requireCode(t, post(), codeOK)
	requireCode(t, post(), codeUnique)
}

func TestEntryTypeValid(t *testing.T) {
	for _, typ := range []string{"submit", "reversal", "adjustment", "release_hold", ""} {
		t.Run(typ, func(t *testing.T) {
			err := inTx(t, func(tx pgx.Tx) error {
				_, err := insertEntry(tx, key(t), typ)
				return err
			})
			requireCode(t, err, codeCheck)
		})
	}
}

func TestCorrections(t *testing.T) {
	available, _ := newCustomer(t)
	original := postDeposit(t, available, "100")

	// mirror posts a correction of `original` with the given type.
	mirror := func(typ string) error {
		return inTx(t, func(tx pgx.Tx) error {
			var id int64
			err := tx.QueryRow(context.Background(),
				`INSERT INTO journal_entries (idempotency_key, type, reverses_entry_id)
				 VALUES ($1, $2, $3) RETURNING id`, key(t), typ, original).Scan(&id)
			if err != nil {
				return err
			}
			if err := insertPosting(tx, id, available, "debit", "100"); err != nil {
				return err
			}
			return insertPosting(tx, id, house["chain_omnibus"], "credit", "100")
		})
	}

	requireCode(t, mirror("deposit"), codeCheck)     // only corrections may reverse
	requireCode(t, mirror("correction"), codeOK)     // first mirror
	requireCode(t, mirror("correction"), codeUnique) // an entry is mirrored at most once
}

// ---- append-only, enforced for every role ----

func TestEntriesAndPostingsAppendOnlyEvenForOwner(t *testing.T) {
	available, _ := newCustomer(t)
	entryID := postDeposit(t, available, "100")
	tests := []string{
		`UPDATE postings SET amount = amount + 1 WHERE entry_id = $1`,
		`DELETE FROM postings WHERE entry_id = $1`,
		`UPDATE journal_entries SET type = 'correction' WHERE id = $1`,
		`DELETE FROM journal_entries WHERE id = $1`,
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), sql, entryID)
			requireCode(t, err, codeNoPrivilege)
		})
	}
	for _, sql := range []string{`TRUNCATE postings`, `TRUNCATE journal_entries CASCADE`} {
		t.Run(sql, func(t *testing.T) {
			_, err := pool.Exec(context.Background(), sql)
			requireCode(t, err, codeNoPrivilege)
		})
	}
}

// ---- least privilege: the ferry_app role ----

func TestAppRolePrivileges(t *testing.T) {
	available, held := newCustomer(t)
	entryID := postDeposit(t, available, "100")

	tests := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{"select postings", `SELECT count(*) FROM postings`, nil, codeOK},
		{"select entries", `SELECT count(*) FROM journal_entries`, nil, codeOK},
		{"lock account rows", `SELECT id FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, []any{[]int64{available, held}}, codeOK},
		{"update balance", `UPDATE accounts SET posted = posted, updated_at = now() WHERE id = $1`, []any{available}, codeOK},
		{"open account", `INSERT INTO accounts (owner_id, kind, currency) VALUES (gen_random_uuid(), 'available', 'USDC')`, nil, codeOK},
		{"update account kind", `UPDATE accounts SET kind = 'held' WHERE id = $1`, []any{available}, codeNoPrivilege},
		{"update account owner", `UPDATE accounts SET owner_id = gen_random_uuid() WHERE id = $1`, []any{available}, codeNoPrivilege},
		{"delete account", `DELETE FROM accounts WHERE id = $1`, []any{available}, codeNoPrivilege},
		{"update posting", `UPDATE postings SET amount = 1 WHERE entry_id = $1`, []any{entryID}, codeNoPrivilege},
		{"delete posting", `DELETE FROM postings WHERE entry_id = $1`, []any{entryID}, codeNoPrivilege},
		{"update entry", `UPDATE journal_entries SET type = 'correction' WHERE id = $1`, []any{entryID}, codeNoPrivilege},
		{"delete entry", `DELETE FROM journal_entries WHERE id = $1`, []any{entryID}, codeNoPrivilege},
		{"truncate postings", `TRUNCATE postings`, nil, codeNoPrivilege},
		{"create table", `CREATE TABLE sneaky (id int)`, nil, codeNoPrivilege},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := asApp(t, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), tc.sql, tc.args...)
				return err
			})
			requireCode(t, err, tc.want)
		})
	}
}

func TestAppRoleCanPostBalancedEntry(t *testing.T) {
	available, held := newCustomer(t)
	postDeposit(t, available, "100")
	err := asApp(t, func(tx pgx.Tx) error {
		id, err := insertEntry(tx, key(t), "hold")
		if err != nil {
			return err
		}
		if err := insertPosting(tx, id, available, "debit", "60"); err != nil {
			return err
		}
		return insertPosting(tx, id, held, "credit", "60")
	})
	requireCode(t, err, codeOK)
}

func TestAppRoleCannotLogIn(t *testing.T) {
	var canLogin bool
	err := pool.QueryRow(context.Background(),
		`SELECT rolcanlogin FROM pg_roles WHERE rolname = 'ferry_app'`).Scan(&canLogin)
	if err != nil {
		t.Fatal(err)
	}
	if canLogin {
		t.Fatal("ferry_app must be NOLOGIN; deployments grant it to a separate login role")
	}
}
