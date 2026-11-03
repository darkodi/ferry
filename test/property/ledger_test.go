// Property test for the ledger: random sequences of deposits, top-ups,
// holds, settlements, releases, bank returns and replayed entries, run
// against a real Postgres. A simple in-memory model predicts every balance;
// after each step the database must agree with the model and pass every
// ledger invariant.
package property

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool  *pgxpool.Pool
	house ledger.HouseAccounts
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("skipping property tests in -short mode")
		os.Exit(0)
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

// model is what the ledger should contain, tracked in plain Go.
type model struct {
	c               ledger.CustomerAccounts
	available, held ledger.Amount
	omnibus0, noah0 ledger.Amount // house balances when this run started
	omnibus, noah   ledger.Amount // change since then
	holds           map[uuid.UUID]ledger.Amount
	settled         map[uuid.UUID]ledger.Amount // payout -> amount that left via Noah
	posted          []ledger.Entry              // every entry accepted so far
}

func post(e ledger.Entry) error {
	return db.WithTx(context.Background(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := ledger.PostEntry(context.Background(), tx, e)
		return err
	})
}

func balance(t *rapid.T, id ledger.AccountID) ledger.Amount {
	b, err := ledger.Balance(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func amount(t *rapid.T, label string, lo, hi ledger.Amount) ledger.Amount {
	return ledger.Amount(rapid.Int64Range(int64(lo), int64(hi)).Draw(t, label))
}

// pick draws one key of m in a stable order, or skips the action if m is empty.
func pick(t *rapid.T, m map[uuid.UUID]ledger.Amount) uuid.UUID {
	if len(m) == 0 {
		t.Skip("nothing to pick")
	}
	keys := make([]uuid.UUID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return rapid.SampledFrom(keys).Draw(t, "payout")
}

func (s *model) mustPost(t *rapid.T, e ledger.Entry) {
	if err := post(e); err != nil {
		t.Fatalf("post %s: %v", e.IdempotencyKey, err)
	}
	s.posted = append(s.posted, e)
}

func TestLedgerProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c, err := ledger.OpenCustomerAccounts(context.Background(), pool, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		s := &model{
			c:        c,
			omnibus0: balance(t, house.ChainOmnibus),
			noah0:    balance(t, house.NoahPrefunded),
			holds:    map[uuid.UUID]ledger.Amount{},
			settled:  map[uuid.UUID]ledger.Amount{},
		}
		const maxAmount = 1_000_000_000_000 // 1,000,000 USDC

		t.Repeat(map[string]func(*rapid.T){
			"deposit": func(t *rapid.T) {
				a := amount(t, "amount", 1, maxAmount)
				s.mustPost(t, ledger.Deposit("evm-test", uuid.NewString(), 0, uuid.New(), house, s.c, a))
				s.available += a
				s.omnibus += a
			},
			"topup": func(t *rapid.T) {
				a := amount(t, "amount", 1, maxAmount)
				s.mustPost(t, ledger.TreasuryTopup(uuid.NewString(), house, a))
				s.noah += a
				s.omnibus -= a
			},
			"hold": func(t *rapid.T) {
				// Up to twice the balance, so roughly half the holds must be refused.
				a := amount(t, "amount", 1, max(1, 2*s.available))
				p := uuid.New()
				err := post(ledger.Hold(p, s.c, a))
				if a > s.available {
					if !errors.Is(err, ledger.ErrInsufficientFunds) {
						t.Fatalf("hold %s of %s: want ErrInsufficientFunds, got %v", a, s.available, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("hold %s of %s: %v", a, s.available, err)
				}
				s.available -= a
				s.held += a
				s.holds[p] = a
			},
			"settle": func(t *rapid.T) {
				p := pick(t, s.holds)
				authorized := s.holds[p]
				actual := amount(t, "actual", 1, authorized)
				fee := amount(t, "fee", 0, authorized-actual)
				e, err := ledger.Settle(p, house, s.c, authorized, actual, fee)
				if err != nil {
					t.Fatal(err)
				}
				s.mustPost(t, e)
				delete(s.holds, p)
				s.settled[p] = actual + fee
				s.held -= authorized
				s.noah -= actual + fee
				s.available += authorized - actual - fee
			},
			"release": func(t *rapid.T) {
				p := pick(t, s.holds)
				a := s.holds[p]
				s.mustPost(t, ledger.Release(p, s.c, a))
				delete(s.holds, p)
				s.held -= a
				s.available += a
			},
			"bank_return": func(t *rapid.T) {
				p := pick(t, s.settled)
				returned := amount(t, "returned", 1, s.settled[p])
				s.mustPost(t, ledger.BankReturn(p, "rtx", house, s.c, returned))
				delete(s.settled, p) // one return per payout in this model
				s.noah += returned
				s.available += returned
			},
			"replay": func(t *rapid.T) {
				if len(s.posted) == 0 {
					t.Skip("nothing posted yet")
				}
				e := rapid.SampledFrom(s.posted).Draw(t, "entry")
				if err := post(e); !errors.Is(err, ledger.ErrDuplicateEntry) {
					t.Fatalf("replay %s: want ErrDuplicateEntry, got %v", e.IdempotencyKey, err)
				}
			},
			"": func(t *rapid.T) {
				for _, chk := range []struct {
					name string
					id   ledger.AccountID
					want ledger.Amount
				}{
					{"available", s.c.Available, s.available},
					{"held", s.c.Held, s.held},
					{"chain_omnibus", house.ChainOmnibus, s.omnibus0 + s.omnibus},
					{"noah_prefunded", house.NoahPrefunded, s.noah0 + s.noah},
				} {
					if got := balance(t, chk.id); got != chk.want {
						t.Fatalf("%s = %s, model says %s", chk.name, got, chk.want)
					}
				}
				vs, err := ledger.CheckInvariants(context.Background(), pool)
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range vs {
					if v.Check != ledger.CheckHouseNegative {
						t.Fatalf("invariant violated: %+v", v)
					}
				}
			},
		})
	})
}
