package payout_test

// Worker and resolver tests: the M3 rows of the design's failure table.
// They share the database and mock rail with service_test.go.

import (
	"context"
	"log/slog"
	"strconv"
	"testing"

	"github.com/darkodi/ferry/internal/customer"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/rail/mock"
)

var discard = slog.New(slog.DiscardHandler)

// held creates and confirms a 100 USD payout (101.50 USDC held).
func held(t *testing.T, c customer.Customer, plan mock.Outcome) payout.Payout {
	t.Helper()
	p := create(t, c, 100_00)
	rl.Plan(p.ID.String(), plan)
	p, err := svc.Confirm(context.Background(), c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, p, payout.Held)
	return p
}

// heldEvent is the payout.held message the relay would publish for p.
func heldEvent(t *testing.T, p payout.Payout) queue.Message {
	t.Helper()
	var id int64
	var body []byte
	err := pool.QueryRow(context.Background(),
		`SELECT id, payload FROM outbox WHERE aggregate_id = $1 AND topic = 'payout.held'`, p.ID).Scan(&id, &body)
	if err != nil {
		t.Fatal(err)
	}
	return queue.Message{ID: strconv.FormatInt(id, 10), Topic: "payout.held", Body: body}
}

func load(t *testing.T, p payout.Payout) payout.Payout {
	t.Helper()
	p, err := svc.Get(context.Background(), p.CustomerID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func railAttempts(t *testing.T, p payout.Payout) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rail_transactions WHERE payout_id = $1`, p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func drainWorker(t *testing.T, w *payout.Worker) {
	t.Helper()
	for range 10 {
		n, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// resolver acts on every held or submitted payout, however recent.
func resolver() *payout.Resolver {
	r := payout.NewResolver(svc, discard)
	r.StaleAfter = 0
	return r
}

func resolve(t *testing.T, r *payout.Resolver) {
	t.Helper()
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Failure table: "Outbox relay crash after publish" (the event arrives
// twice) and "Duplicate webhook"-style redelivery: one rail transaction.
func TestWorkerSubmitsOnceForDuplicateEvents(t *testing.T) {
	c := newCustomer(t, "200")
	p := held(t, c, mock.Default)
	q := queue.NewMemory()
	ev := heldEvent(t, p)
	for range 3 {
		if err := q.Publish(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	drainWorker(t, payout.NewWorker(svc, q, discard))

	wantStatus(t, load(t, p), payout.Submitted)
	if n := railAttempts(t, p); n != 1 {
		t.Fatalf("%d rail attempts, want 1", n)
	}
	if q.Len() != 0 || len(q.Dead()) != 0 {
		t.Fatalf("queue: %d left, %d dead; want all acknowledged", q.Len(), len(q.Dead()))
	}
	wantBalances(t, c, "98.5", "101.5") // submitting posts nothing
}

// Failure table: "Poison message" → never acknowledged, so the queue
// dead-letters it after MaxReceives deliveries.
func TestWorkerDeadLettersBadMessages(t *testing.T) {
	if pool == nil {
		t.Skip("database tests need Docker; skipped with -short")
	}
	q := queue.NewMemory()
	for _, m := range []queue.Message{
		{ID: "bad-1", Topic: "payout.held", Body: []byte("not json")},
		{ID: "bad-2", Topic: "payout.held", Body: []byte(`{"payout_id":"00000000-0000-0000-0000-000000000001"}`)}, // unknown payout
		{ID: "ok-1", Topic: "payout.settled", Body: []byte(`{}`)},                                                 // no work, acknowledged
	} {
		if err := q.Publish(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	drainWorker(t, payout.NewWorker(svc, q, discard))
	dead := q.Dead()
	if len(dead) != 2 || dead[0].ID != "bad-1" || dead[1].ID != "bad-2" {
		t.Fatalf("dead letters = %+v, want bad-1 and bad-2", dead)
	}
	if q.Len() != 0 {
		t.Fatalf("%d messages left in the queue", q.Len())
	}
}

// Failure table: "Rail call times out" and "Worker dies after rail call,
// before DB commit". The rail has the transaction, Ferry does not know;
// the resolver resends with the same nonce and gets the same transaction.
func TestResolverResolvesUnknownOutcome(t *testing.T) {
	c := newCustomer(t, "200")
	p := held(t, c, mock.Timeout)
	q := queue.NewMemory()
	if err := q.Publish(context.Background(), heldEvent(t, p)); err != nil {
		t.Fatal(err)
	}
	drainWorker(t, payout.NewWorker(svc, q, discard))
	p = load(t, p)
	wantStatus(t, p, payout.Held)
	if !p.NeedsResolution {
		t.Fatal("NeedsResolution not set after the timeout")
	}

	resolve(t, resolver())
	p = load(t, p)
	wantStatus(t, p, payout.Submitted)
	if p.NeedsResolution {
		t.Fatal("NeedsResolution still set")
	}
	if n := railAttempts(t, p); n != 1 {
		t.Fatalf("%d rail attempts, want 1", n)
	}
	wantBalances(t, c, "98.5", "101.5")
}

// Failure table: "Webhook never arrives" and "Compliance review". The
// resolver re-fetches stuck payouts; a long review is not a failure, and
// nothing is cancelled while it lasts.
func TestResolverWaitsOutReviewThenSettles(t *testing.T) {
	c := newCustomer(t, "200")
	p := submitted(t, c)
	id := railTxID(t, p)
	if err := rl.SetSubStatus(id, "review"); err != nil {
		t.Fatal(err)
	}
	r := resolver()
	for range 3 {
		resolve(t, r)
	}
	wantStatus(t, load(t, p), payout.Submitted)
	wantBalances(t, c, "98.5", "101.5")

	// Approved and settled, but the webhook is lost: the resolver finds it.
	if err := rl.Settle(id, usdc("100"), usdc("0.5")); err != nil {
		t.Fatal(err)
	}
	resolve(t, r)
	wantStatus(t, load(t, p), payout.Settled)
	wantBalances(t, c, "99.5", "0")
}
