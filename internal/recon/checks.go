package recon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/payout"
	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/recon/recondb"
)

// ---- internal ----

// Internal turns the ledger and payout invariants into breaks.
type Internal struct {
	db ledger.DB
}

// NewInternal returns the internal checker.
func NewInternal(db ledger.DB) *Internal { return &Internal{db: db} }

// Check implements Checker.
func (*Internal) Check() string { return "internal" }

// Scope implements Checker.
func (*Internal) Scope() string { return "" }

// Run implements Checker.
func (c *Internal) Run(ctx context.Context, _ Window) ([]Break, []Match, error) {
	lv, err := ledger.CheckInvariants(ctx, c.db)
	if err != nil {
		return nil, nil, err
	}
	var out []Break
	for _, v := range lv {
		subject := "ledger"
		if v.Account != 0 {
			subject = "account:" + strconv.FormatInt(int64(v.Account), 10)
		}
		out = append(out, Break{Check: "internal", Kind: v.Check, Subject: subject, Detail: map[string]string{"detail": v.Detail}})
	}
	pv, err := payout.CheckInvariants(ctx, c.db)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range pv {
		out = append(out, Break{Check: "internal", Kind: v.Check, Subject: v.ID.String(), Detail: map[string]string{"detail": v.Detail}})
	}
	return out, nil, nil
}

// ---- chain ----

// BalanceReader reads USDC balances of deposit addresses on one chain.
type BalanceReader interface {
	// Chain is the chain name, as in deposit_addresses.
	Chain() string
	// Point picks where balances are read, given the watcher's scan
	// cursor: a block (EVM, at most the cursor and confirmed) or
	// math.MaxInt64 for "now" (Solana, finalized).
	Point(ctx context.Context, cursor uint64) (uint64, error)
	// Balance is the USDC (minor units) held at address at point.
	Balance(ctx context.Context, address string, point uint64) (ledger.Amount, error)
}

// Chain checks one chain's deposit addresses against the deposits Ferry
// recorded, and those deposits against the ledger.
type Chain struct {
	db recondb.DBTX
	r  BalanceReader
}

// NewChain returns the checker for one chain.
func NewChain(db recondb.DBTX, r BalanceReader) *Chain { return &Chain{db: db, r: r} }

// Check implements Checker.
func (*Chain) Check() string { return "chain" }

// Scope implements Checker.
func (c *Chain) Scope() string { return c.r.Chain() + ":" }

// Run implements Checker.
//
// Deposit addresses are never swept yet, so each one's on-chain balance
// must equal the deposits Ferry recorded there (pending or confirmed, in
// blocks up to the reading point). A difference means a transfer the
// watcher missed or recorded wrongly, or USDC that arrived from before the
// watcher started.
func (c *Chain) Run(ctx context.Context, _ Window) ([]Break, []Match, error) {
	chain := c.r.Chain()
	q := recondb.New(c.db)
	cursor, err := q.ChainCursor(ctx, chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil // the watcher has not started on this chain
	}
	if err != nil {
		return nil, nil, fmt.Errorf("recon: %s cursor: %w", chain, err)
	}
	point, err := c.r.Point(ctx, uint64(cursor)) //nolint:gosec // block numbers are stored from uint64
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.RecordedDeposits(ctx, recondb.RecordedDepositsParams{Chain: chain, MaxBlock: int64(min(point, math.MaxInt64))})
	if err != nil {
		return nil, nil, fmt.Errorf("recon: %s deposits: %w", chain, err)
	}
	recorded := make(map[string]ledger.Amount, len(rows))
	for _, r := range rows {
		recorded[r.Address] = ledger.Amount(r.Total)
	}
	addrs, err := q.ChainAddresses(ctx, chain)
	if err != nil {
		return nil, nil, fmt.Errorf("recon: %s addresses: %w", chain, err)
	}
	var out []Break
	for _, a := range addrs {
		have, err := c.r.Balance(ctx, a, point)
		if err != nil {
			return nil, nil, err
		}
		if want := recorded[a]; have != want {
			out = append(out, Break{Check: "chain", Kind: "address_balance", Subject: chain + ":" + a,
				Detail: map[string]string{"on_chain": have.String(), "recorded": want.String()}})
		}
	}

	confirmed, err := q.ConfirmedDepositTotal(ctx, chain)
	if err != nil {
		return nil, nil, fmt.Errorf("recon: %s confirmed deposits: %w", chain, err)
	}
	posted, err := q.LedgerDepositTotal(ctx, "deposit:"+chain+":")
	if err != nil {
		return nil, nil, fmt.Errorf("recon: %s ledger deposits: %w", chain, err)
	}
	if confirmed != posted {
		out = append(out, Break{Check: "chain", Kind: "ledger_deposits", Subject: chain + ":ledger",
			Detail: map[string]string{"confirmed": ledger.Amount(confirmed).String(), "ledger": ledger.Amount(posted).String()}})
	}
	return out, nil, nil
}

// ---- rail ----

// Rail checks the rail's transactions and balance against Ferry.
type Rail struct {
	db    recondb.DBTX
	name  string
	src   rail.Reconcilable
	house ledger.HouseAccounts
}

// NewRail returns the checker for the named rail (e.g. "noah").
func NewRail(db recondb.DBTX, name string, src rail.Reconcilable, house ledger.HouseAccounts) *Rail {
	return &Rail{db: db, name: name, src: src, house: house}
}

// Check implements Checker.
func (*Rail) Check() string { return "rail" }

// Scope implements Checker.
func (c *Rail) Scope() string { return c.name + ":" }

func (c *Rail) brk(kind, subject string, detail map[string]string) Break {
	return Break{Check: "rail", Kind: kind, Subject: c.name + ":" + subject, Detail: detail}
}

// Run implements Checker. Every rail transaction in the window must be
// explained by Ferry, and every Ferry rail attempt in the window must
// exist at the rail:
//
//   - a sale (Out) belongs to a payout, in an agreeing state, and a
//     settled one was booked for exactly its amount;
//   - a return (In, Reverses) was booked for exactly its amount;
//   - any other credit (In) was booked as a treasury top-up;
//   - the rail's balance equals the ledger's prefunded account.
func (c *Rail) Run(ctx context.Context, w Window) ([]Break, []Match, error) {
	recs, err := c.src.Records(ctx, w.From, w.To)
	if err != nil {
		return nil, nil, err
	}
	q := recondb.New(c.db)
	var out []Break
	var matches []Match
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		seen[r.ID] = true
		switch {
		case r.Direction == "Out" && r.Reverses == "":
			b, m, err := c.sale(ctx, q, r)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, b...)
			matches = append(matches, m...)
		case r.Direction == "In" && r.Reverses != "":
			b, err := c.ret(ctx, q, r)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, b...)
		case r.Direction == "In":
			if r.Status != rail.StatusSettled {
				continue
			}
			booked, err := c.entry(ctx, q, "topup:"+c.name+":"+r.ID, "debit")
			if err != nil {
				return nil, nil, err
			}
			if booked != r.Amount {
				out = append(out, c.brk("topup_not_booked", r.ID, map[string]string{"rail": r.Amount.String(), "ledger": booked.String()}))
			}
		default:
			out = append(out, c.brk("unexplained_rail_tx", r.ID, map[string]string{"direction": r.Direction, "status": string(r.Status), "amount": r.Amount.String()}))
		}
	}

	// Ferry's attempts in the window that the listing did not show: look
	// each up before calling it missing (creation times differ slightly).
	mine, err := q.RailTxsInWindow(ctx, recondb.RailTxsInWindowParams{Rail: c.name, FromTime: w.From, ToTime: w.To})
	if err != nil {
		return nil, nil, fmt.Errorf("recon: rail attempts: %w", err)
	}
	for _, m := range mine {
		if m.RailTxID == nil || seen[*m.RailTxID] {
			continue // no rail id yet: the resolver's job
		}
		_, found, err := c.src.Record(ctx, *m.RailTxID)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			out = append(out, c.brk("missing_at_rail", *m.RailTxID, map[string]string{"payout_id": m.PayoutID.String(), "payout_status": m.PayoutStatus}))
		}
	}

	have, err := c.src.Balance(ctx)
	if err != nil {
		return nil, nil, err
	}
	want, err := ledger.Balance(ctx, c.db, c.house.NoahPrefunded)
	if err != nil {
		return nil, nil, fmt.Errorf("recon: prefunded balance: %w", err)
	}
	if have != want {
		out = append(out, c.brk("balance", "balance", map[string]string{"rail": have.String(), "ledger": want.String()}))
	}
	return out, matches, nil
}

// sale checks one outgoing rail transaction.
func (c *Rail) sale(ctx context.Context, q *recondb.Queries, r rail.Record) ([]Break, []Match, error) {
	id := r.ID
	row, err := q.RailTxByRailID(ctx, recondb.RailTxByRailIDParams{Rail: c.name, RailTxID: &id})
	if errors.Is(err, pgx.ErrNoRows) {
		// The answer to the submit may have been lost: try the payout id.
		if pid, perr := uuid.Parse(r.ExternalID); perr == nil {
			var r2 recondb.RailTxByPayoutRow
			r2, err = q.RailTxByPayout(ctx, recondb.RailTxByPayoutParams{Rail: c.name, PayoutID: pid})
			row = recondb.RailTxByRailIDRow(r2)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return []Break{c.brk("unknown_rail_tx", r.ID, map[string]string{
			"direction": r.Direction, "status": string(r.Status), "amount": r.Amount.String(), "external_id": r.ExternalID,
		})}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("recon: match rail tx %s: %w", r.ID, err)
	}
	var out []Break
	if !agrees(r.Status, row.PayoutStatus) {
		out = append(out, c.brk("status_mismatch", r.ID, map[string]string{"rail": string(r.Status), "payout": row.PayoutStatus}))
	}
	if r.Status == rail.StatusSettled {
		booked, err := c.entry(ctx, q, "payout:"+row.PayoutID.String()+":settle", "credit")
		if err != nil {
			return nil, nil, err
		}
		if booked != r.Amount && (row.PayoutStatus == "settled" || row.PayoutStatus == "reversed") {
			out = append(out, c.brk("settle_amount", r.ID, map[string]string{"rail": r.Amount.String(), "ledger": booked.String()}))
		}
	}
	return out, []Match{{Ferry: row.PayoutID.String(), External: r.ID}}, nil
}

// ret checks one return (an incoming transaction that reverses a sale).
func (c *Rail) ret(ctx context.Context, q *recondb.Queries, r rail.Record) ([]Break, error) {
	orig := r.Reverses
	row, err := q.RailTxByRailID(ctx, recondb.RailTxByRailIDParams{Rail: c.name, RailTxID: &orig})
	if errors.Is(err, pgx.ErrNoRows) {
		return []Break{c.brk("unknown_rail_tx", r.ID, map[string]string{
			"direction": r.Direction, "reverses": r.Reverses, "amount": r.Amount.String(),
		})}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recon: match return %s: %w", r.ID, err)
	}
	if r.Status != rail.StatusSettled {
		return nil, nil
	}
	booked, err := c.entry(ctx, q, "payout:"+row.PayoutID.String()+":return:"+r.ID, "debit")
	if err != nil {
		return nil, err
	}
	if booked != r.Amount {
		return []Break{c.brk("return_not_booked", r.ID, map[string]string{
			"payout_id": row.PayoutID.String(), "rail": r.Amount.String(), "ledger": booked.String(), "payout": row.PayoutStatus,
		})}, nil
	}
	return nil, nil
}

func (c *Rail) entry(ctx context.Context, q *recondb.Queries, key, direction string) (ledger.Amount, error) {
	a, err := q.EntryAmount(ctx, recondb.EntryAmountParams{IdempotencyKey: key, Direction: direction, Kind: string(ledger.KindNoahPrefunded)})
	if err != nil {
		return 0, fmt.Errorf("recon: ledger entry %s: %w", key, err)
	}
	return ledger.Amount(a), nil
}

// agrees reports whether a rail status and a payout state can both be
// true at once. Pending at the rail is a payout still held (its submit
// answer was lost) or submitted.
func agrees(s rail.Status, payoutStatus string) bool {
	switch s {
	case rail.StatusPending:
		return payoutStatus == "held" || payoutStatus == "submitted"
	case rail.StatusSettled:
		return payoutStatus == "settled" || payoutStatus == "reversed"
	case rail.StatusFailed:
		return payoutStatus == "failed"
	default:
		return false
	}
}
