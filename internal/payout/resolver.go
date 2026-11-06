package payout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/payout/payoutdb"
)

// Resolver finds payouts that stopped moving and pushes them on. It never
// gives up on a payout or cancels one: it only asks the rail again.
//
//   - held for longer than StaleAfter: the submit event was lost (or sits
//     in the dead-letter queue), or the submit outcome was unknown
//     (NeedsResolution). Submit again; the stored nonce makes this safe.
//   - submitted for longer than StaleAfter: a webhook may have been missed.
//     Sync re-fetches the rail transaction and applies anything new. A
//     payout under compliance review simply stays submitted.
//
// Several resolvers may run at once: every transition is version-guarded,
// so the loser of a race changes nothing.
type Resolver struct {
	svc        *Service
	log        *slog.Logger
	StaleAfter time.Duration
	Batch      int32
}

// NewResolver returns a resolver that acts on payouts idle for a minute.
func NewResolver(svc *Service, log *slog.Logger) *Resolver {
	return &Resolver{svc: svc, log: log, StaleAfter: time.Minute, Batch: 100}
}

// Run calls RunOnce every interval until ctx is cancelled.
func (r *Resolver) Run(ctx context.Context, interval time.Duration) {
	for {
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.ErrorContext(ctx, "payout resolver", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// RunOnce handles one batch of stale held and submitted payouts and
// returns how many it looked at. A payout that fails is logged and
// skipped; it is picked up again next round.
func (r *Resolver) RunOnce(ctx context.Context) (int, error) {
	staleSecs := int32(math.MaxInt32)
	if s := r.StaleAfter / time.Second; s < math.MaxInt32 {
		staleSecs = int32(max(s, 0))
	}
	total := 0
	for _, pass := range []struct {
		status State
		act    func(context.Context, uuid.UUID) (Payout, error)
	}{
		{Held, r.svc.Submit},
		{Submitted, r.svc.Sync},
	} {
		ids, err := payoutdb.New(r.svc.pool).StaleInFlight(ctx, payoutdb.StaleInFlightParams{
			Status: string(pass.status), StaleSecs: staleSecs, MaxRows: r.Batch,
		})
		if err != nil {
			return total, fmt.Errorf("payout resolver: list %s: %w", pass.status, err)
		}
		for _, id := range ids {
			total++
			p, err := pass.act(ctx, id)
			switch {
			case errors.Is(err, ErrRailMismatch):
				r.log.ErrorContext(ctx, "payout resolver: rail mismatch, needs a human", "payout_id", id, "error", err)
			case err != nil:
				r.log.WarnContext(ctx, "payout resolver", "payout_id", id, "status", pass.status, "error", err)
			case p.Status != pass.status:
				r.log.InfoContext(ctx, "payout resolved", "payout_id", id, "from", pass.status, "to", p.Status)
			}
		}
	}
	return total, nil
}
