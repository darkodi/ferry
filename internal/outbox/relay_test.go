package outbox_test

// Relay tests on a throwaway Postgres with the in-memory queue. Skipped
// with -short.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/platform/queue"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

var (
	pool *pgxpool.Pool
	log  = slog.New(slog.NewTextHandler(io.Discard, nil))
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
	return m.Run()
}

// clock only moves when told to.
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

func newClock() *clock { return &clock{t: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)} }

// write adds n events to the outbox, each in its own transaction.
func write(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		err := db.WithTx(t.Context(), pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			return outbox.Write(t.Context(), tx, "test.event", uuid.New(), map[string]int{"n": i})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// drain publishes anything earlier tests left behind, so each test starts
// with an empty outbox.
func drain(t *testing.T) {
	t.Helper()
	r := outbox.NewRelay(pool, queue.NewMemory(), newClock().Now, log)
	r.Lease = 0
	for {
		n, err := r.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
}

func TestRelayPublishesOnce(t *testing.T) {
	skipShort(t)
	drain(t)
	write(t, 3)
	q := queue.NewMemory()
	r := outbox.NewRelay(pool, q, newClock().Now, log)

	n, err := r.RunOnce(t.Context())
	if err != nil || n != 3 {
		t.Fatalf("first round: n=%d err=%v, want 3 published", n, err)
	}
	if n, err := r.RunOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("second round: n=%d err=%v, want nothing left", n, err)
	}
	got, _ := q.Receive(t.Context(), 10)
	if len(got) != 3 {
		t.Fatalf("queue has %d messages, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		prev, _ := strconv.ParseInt(got[i-1].ID, 10, 64)
		cur, _ := strconv.ParseInt(got[i].ID, 10, 64)
		if prev >= cur {
			t.Errorf("messages out of order: %d before %d", prev, cur)
		}
	}
	if got[0].Topic != "test.event" {
		t.Errorf("topic = %q", got[0].Topic)
	}
}

// lossyPublisher stores every message but reports failure, like a queue
// that accepted the message and then timed out before answering.
type lossyPublisher struct {
	*queue.Memory
	fail bool
}

func (p *lossyPublisher) Publish(ctx context.Context, m queue.Message) error {
	if err := p.Memory.Publish(ctx, m); err != nil {
		return err
	}
	if p.fail {
		return errors.New("timeout")
	}
	return nil
}

// Failure table: "Outbox relay crash after publish" → duplicate event.
// The row stays leased until the lease runs out, then goes out again with
// the same message id, which consumers dedup on.
func TestRelayRepublishesAfterLostAck(t *testing.T) {
	skipShort(t)
	drain(t)
	write(t, 1)
	clk := newClock()
	q := &lossyPublisher{Memory: queue.NewMemory(), fail: true}
	r := outbox.NewRelay(pool, q, clk.Now, log)

	if _, err := r.RunOnce(t.Context()); err == nil {
		t.Fatal("want a publish error")
	}
	q.fail = false
	if n, _ := r.RunOnce(t.Context()); n != 0 {
		t.Fatalf("published %d rows while the lease was held", n)
	}
	clk.Add(r.Lease + time.Second)
	if n, err := r.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("after the lease: n=%d err=%v, want 1", n, err)
	}
	got, _ := q.Receive(t.Context(), 10)
	if len(got) != 2 || got[0].ID != got[1].ID {
		t.Fatalf("want the same message twice, got %+v", got)
	}
}

// Two relays at once never publish the same row within a lease.
func TestRelaysDoNotOverlap(t *testing.T) {
	skipShort(t)
	drain(t)
	const rows = 200
	write(t, rows)
	q := queue.NewMemory()
	clk := newClock()

	var wg sync.WaitGroup
	for range 4 {
		r := outbox.NewRelay(pool, q, clk.Now, log)
		r.Batch = 10
		wg.Go(func() {
			for {
				n, err := r.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()

	got, _ := q.Receive(t.Context(), 1000)
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.ID] {
			t.Fatalf("message %s published twice", d.ID)
		}
		seen[d.ID] = true
	}
	if len(seen) != rows {
		t.Fatalf("published %d distinct rows, want %d", len(seen), rows)
	}
}
