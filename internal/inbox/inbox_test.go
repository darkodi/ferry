package inbox_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/inbox"
	"github.com/darkodi/ferry/internal/testutil/pgtest"
)

// A message delivered several times, even concurrently, runs its effect
// once. Failure table: "Duplicate webhook" / "Outbox relay crash after
// publish" both rely on this.
func TestHandleRunsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	ctx := context.Background()
	tdb, err := pgtest.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tdb.Close)
	pool, err := pgxpool.New(ctx, tdb.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	var runs, fresh atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			ok, err := inbox.Handle(ctx, pool, "test", "msg-1", func(pgx.Tx) error {
				runs.Add(1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			if ok {
				fresh.Add(1)
			}
		})
	}
	wg.Wait()
	if runs.Load() != 1 || fresh.Load() != 1 {
		t.Fatalf("effect ran %d times (%d fresh), want once", runs.Load(), fresh.Load())
	}

	seen, err := inbox.Seen(ctx, pool, "test", "msg-1")
	if err != nil || !seen {
		t.Fatalf("Seen = %v, %v; want true", seen, err)
	}
	// Another consumer has its own record of the same message.
	seen, err = inbox.Seen(ctx, pool, "other", "msg-1")
	if err != nil || seen {
		t.Fatalf("other consumer Seen = %v, %v; want false", seen, err)
	}
}
