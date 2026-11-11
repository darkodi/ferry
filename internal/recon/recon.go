// Package recon reconciles Ferry against the outside world and itself.
//
// Each run asks every checker for the disagreements it can see now:
//
//   - internal: the ledger and payout invariants;
//   - chain:    on-chain USDC at each deposit address against the deposits
//     Ferry recorded there, and those deposits against the ledger;
//   - rail:     the rail's transactions and balance against Ferry's rail
//     attempts, payouts and ledger.
//
// A disagreement is a break, identified by (check, kind, subject). Breaks
// are upserted, so one that lasts across runs is one row with a growing
// seen_count; one that a later complete run no longer sees is cleared.
// Point-in-time checks (a deposit on-chain but not yet scanned, a payout
// settled at the rail a moment before its webhook) therefore clear
// themselves, and alerts look only at breaks that persist.
//
// Reconciliation never changes the ledger or any payout. A person
// resolves a break with a reason; a ledger fix is a separate correction
// entry that names the break.
package recon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/recon/recondb"
)

// Break is one disagreement. Detail must be deterministic for the same
// disagreement (no timestamps or block numbers): a resolved break stays
// resolved only while its detail is unchanged.
type Break struct {
	Check   string // internal, chain, rail
	Kind    string
	Subject string
	Detail  map[string]string
}

// Match is a pair that agreed, kept for audit.
type Match struct {
	Ferry, External string
}

// Window is the period a rail check covers, [From, To).
type Window struct {
	From, To time.Time
}

// Checker is one family of checks over one source.
type Checker interface {
	// Check is internal, chain or rail.
	Check() string
	// Scope prefixes every subject this checker reports (e.g.
	// "evm-local:"), so a run clears only the breaks of checkers that
	// completed.
	Scope() string
	Run(ctx context.Context, w Window) ([]Break, []Match, error)
}

// Recon runs checkers and records their breaks.
type Recon struct {
	pool     *pgxpool.Pool
	checkers []Checker
	log      *slog.Logger
}

// New returns a reconciler over the given checkers.
func New(pool *pgxpool.Pool, log *slog.Logger, checkers ...Checker) *Recon {
	return &Recon{pool: pool, checkers: checkers, log: log}
}

// Report summarises a run.
type Report struct {
	RunID     int64
	Completed []string // check:scope of checkers that ran
	Failed    []string // and of those that could not
	Breaks    int      // breaks seen in this run
	New       int      // of which first seen now
	Cleared   int64    // earlier breaks no longer seen
}

// ErrBusy means another reconciliation run holds the lock.
var ErrBusy = errors.New("recon: another run is in progress")

// Run runs every checker once. A checker that fails (its source is
// unreachable) is reported and skipped; its breaks are left as they are.
func (r *Recon) Run(ctx context.Context, w Window) (Report, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("recon: acquire lock connection: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('ferry:recon'))`).Scan(&locked); err != nil {
		return Report{}, fmt.Errorf("recon: lock: %w", err)
	}
	if !locked {
		return Report{}, ErrBusy
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('ferry:recon'))`) }()

	q := recondb.New(r.pool)
	runID, err := q.InsertRun(ctx, recondb.InsertRunParams{WindowFrom: w.From, WindowTo: w.To})
	if err != nil {
		return Report{}, fmt.Errorf("recon: start run: %w", err)
	}
	rep := Report{RunID: runID, Completed: []string{}, Failed: []string{}}
	for _, c := range r.checkers {
		name := c.Check() + ":" + c.Scope()
		breaks, matches, err := c.Run(ctx, w)
		if err != nil {
			if ctx.Err() != nil {
				return rep, fmt.Errorf("recon: %w", ctx.Err())
			}
			r.log.ErrorContext(ctx, "recon check could not run", "check", name, "error", err)
			rep.Failed = append(rep.Failed, name)
			continue
		}
		fresh, cleared, err := r.record(ctx, runID, c, breaks, matches)
		if err != nil {
			return rep, err
		}
		rep.Completed = append(rep.Completed, name)
		rep.Breaks += len(breaks)
		rep.New += fresh
		rep.Cleared += cleared
	}
	err = q.FinishRun(ctx, recondb.FinishRunParams{
		ID: runID, Checks: rep.Completed, Failed: rep.Failed, BreaksSeen: int32(min(rep.Breaks, 1<<30)), //nolint:gosec // bounded above
	})
	if err != nil {
		return rep, fmt.Errorf("recon: finish run: %w", err)
	}
	return rep, nil
}

// record stores one checker's breaks and matches and clears its breaks
// that were not seen again, in one transaction.
func (r *Recon) record(ctx context.Context, runID int64, c Checker, breaks []Break, matches []Match) (fresh int, cleared int64, err error) {
	err = db.WithTx(ctx, r.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		fresh, cleared = 0, 0
		q := recondb.New(tx)
		for _, b := range breaks {
			if b.Check != c.Check() || len(b.Subject) < len(c.Scope()) || b.Subject[:len(c.Scope())] != c.Scope() {
				return fmt.Errorf("recon: %s:%s reported a break outside its scope: %s %s", c.Check(), c.Scope(), b.Check, b.Subject)
			}
			detail, err := json.Marshal(b.Detail)
			if err != nil {
				return fmt.Errorf("recon: encode break detail: %w", err)
			}
			row, err := q.UpsertBreak(ctx, recondb.UpsertBreakParams{
				ID: uuid.New(), CheckName: b.Check, Kind: b.Kind, Subject: b.Subject, Detail: detail, RunID: runID,
			})
			if err != nil {
				return fmt.Errorf("recon: store break %s/%s: %w", b.Kind, b.Subject, err)
			}
			if row.Inserted {
				fresh++
				r.log.WarnContext(ctx, "recon break", "check", b.Check, "kind", b.Kind, "subject", b.Subject, "detail", b.Detail)
			}
		}
		for _, m := range matches {
			if err := q.InsertMatch(ctx, recondb.InsertMatchParams{
				RunID: runID, CheckName: c.Check(), FerryRef: m.Ferry, ExternalRef: m.External,
			}); err != nil {
				return fmt.Errorf("recon: store match: %w", err)
			}
		}
		n, err := q.ClearUnseen(ctx, recondb.ClearUnseenParams{CheckName: c.Check(), Prefix: c.Scope(), RunID: runID})
		if err != nil {
			return fmt.Errorf("recon: clear breaks: %w", err)
		}
		cleared = n
		return nil
	})
	return fresh, cleared, err
}
