package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/chain/chaindb"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/platform/obs"
)

// Watcher turns one chain's transfers into credited deposits.
//
// Each round (RunOnce):
//
//  0. Provision. If the chain needs on-chain accounts for its addresses
//     (Provisioner set), create the missing ones and mark them active.
//  1. Reorg check. If the block the cursor points at has a different hash
//     now, walk the stored hashes back to the last block both forks share,
//     orphan the pending deposits above it and move the cursor there.
//  2. Scan. Read transfers to watched addresses in the next block range
//     and store them as pending, together with the new cursor, in one
//     transaction. A crash before the commit re-scans the range; a crash
//     after it does not, and the unique key absorbs any overlap.
//  3. Confirm. For each pending deposit at least Confirmations blocks
//     deep, check that its block is still on the chain, then credit it:
//     status, ledger entry and outbox event in one transaction. A deposit
//     whose block is gone is orphaned instead, and the scan rewinds so the
//     transfer is found again if it was re-included elsewhere.
//
// Nothing is credited before step 3, so a reorg shallower than the
// confirmation depth never needs undoing.
type Watcher struct {
	pool  *pgxpool.Pool
	chain Adapter
	house ledger.HouseAccounts
	log   *slog.Logger

	// Confirmations is how deep a block must be before its deposits are
	// credited. 0 credits on sight (Solana reads only finalized slots).
	Confirmations uint64
	// StartBlock is where a chain with no cursor yet starts scanning; 0
	// means the current head (history before it is ignored).
	StartBlock uint64
	// Batch is the most blocks scanned per round.
	Batch uint64
	// KeepBlocks is how many recent block hashes are kept for reorg
	// detection; a reorg deeper than this cannot be rewound automatically.
	KeepBlocks uint64
	// Provisioner, if set, creates on-chain accounts for addresses still
	// "provisioning" at the start of each round.
	Provisioner Provisioner
}

// NewWatcher returns a watcher with batches of 500 blocks and 1024 stored
// hashes. Set Confirmations and StartBlock before running it.
func NewWatcher(pool *pgxpool.Pool, a Adapter, house ledger.HouseAccounts, log *slog.Logger) *Watcher {
	return &Watcher{pool: pool, chain: a, house: house, log: log.With("chain", a.Chain()), Batch: 500, KeepBlocks: 1024}
}

// ErrDeepReorg means the chain no longer contains any block whose hash
// Ferry stored. The watcher stops; a human must look.
var ErrDeepReorg = errors.New("chain: reorg deeper than stored block history")

// Result summarises one round.
type Result struct {
	Provisioned int // addresses made active
	Rewound     bool
	Scanned     uint64 // blocks scanned
	Found       int    // new or revived pending deposits
	Confirmed   int
	Orphaned    int
}

// Run holds the chain's leader lock and runs rounds every interval until
// ctx is cancelled. Only one watcher per chain is active; another one
// waits for the lock.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) error {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("chain: acquire lock connection: %w", err)
	}
	defer conn.Release()
	for {
		var ok bool
		err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, "watcher:"+w.chain.Chain()).Scan(&ok)
		if err != nil {
			return fmt.Errorf("chain: leader lock: %w", err)
		}
		if ok {
			break
		}
		w.log.InfoContext(ctx, "another watcher holds the lock; standing by")
		if !sleep(ctx, 5*interval) {
			return nil
		}
	}
	// Closing the session at Release would free the lock anyway; unlock
	// explicitly so a pooled connection does not keep it.
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, "watcher:"+w.chain.Chain())
	}()
	w.log.InfoContext(ctx, "watcher is the leader")

	for {
		res, err := w.RunOnce(ctx)
		w.record(res, err)
		switch {
		case errors.Is(err, ErrDeepReorg):
			return err
		case err != nil && ctx.Err() == nil:
			w.log.ErrorContext(ctx, "watcher round", "error", err)
		case res.Provisioned+res.Found+res.Confirmed+res.Orphaned > 0 || res.Rewound:
			w.log.InfoContext(ctx, "watcher round", "provisioned", res.Provisioned, "scanned", res.Scanned, "found", res.Found,
				"confirmed", res.Confirmed, "orphaned", res.Orphaned, "rewound", res.Rewound)
		}
		// A full batch means there is more to catch up on.
		if err == nil && res.Scanned == w.Batch {
			continue
		}
		if !sleep(ctx, interval) {
			return nil
		}
	}
}

// record updates the watcher's metrics after a round.
func (w *Watcher) record(res Result, err error) {
	chain := w.chain.Chain()
	switch {
	case errors.Is(err, ErrDeepReorg):
		obs.WatcherDeepReorgs.WithLabelValues(chain).Inc()
		obs.WatcherRounds.WithLabelValues(chain, "error").Inc()
	case err != nil:
		obs.WatcherRounds.WithLabelValues(chain, "error").Inc()
	default:
		obs.WatcherRounds.WithLabelValues(chain, "ok").Inc()
	}
	obs.WatcherDeposits.WithLabelValues(chain, "found").Add(float64(res.Found))
	obs.WatcherDeposits.WithLabelValues(chain, "confirmed").Add(float64(res.Confirmed))
	obs.WatcherDeposits.WithLabelValues(chain, "orphaned").Add(float64(res.Orphaned))
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// RunOnce runs one round. It does not take the leader lock (tests call it
// directly); Run does.
func (w *Watcher) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	var err error
	// 0. Provision.
	if w.Provisioner != nil {
		if res.Provisioned, err = w.provision(ctx); err != nil {
			return res, err
		}
	}

	head, err := w.chain.Head(ctx)
	if err != nil {
		return res, fmt.Errorf("chain: head: %w", err)
	}
	cur, err := w.cursor(ctx, head)
	if err != nil {
		return res, err
	}

	// 1. Reorg check.
	hash, err := w.chain.BlockHash(ctx, cur.number)
	if err != nil {
		return res, fmt.Errorf("chain: hash of cursor block %d: %w", cur.number, err)
	}
	if hash != cur.hash {
		if cur, err = w.rewind(ctx, cur); err != nil {
			return res, err
		}
		res.Rewound = true
	}

	// 2. Scan.
	if head.Number > cur.number {
		to := min(head.Number, cur.number+w.Batch)
		if res.Found, err = w.scan(ctx, cur.number+1, to); err != nil {
			return res, err
		}
		res.Scanned = to - cur.number
	}

	// How far behind the head the scan is, after this round.
	scanned := cur.number + res.Scanned
	if head.Number >= scanned {
		obs.WatcherLag.WithLabelValues(w.chain.Chain()).Set(float64(head.Number - scanned))
	}

	// 3. Confirm.
	if head.Number >= w.Confirmations {
		res.Confirmed, res.Orphaned, err = w.confirm(ctx, head.Number-w.Confirmations)
	}
	return res, err
}

type cursor struct {
	number uint64
	hash   string
}

// cursor loads the scan position, creating it at StartBlock-1 (or the
// head) the first time.
func (w *Watcher) cursor(ctx context.Context, head Block) (cursor, error) {
	q := chaindb.New(w.pool)
	c, err := q.GetCursor(ctx, w.chain.Chain())
	if err == nil {
		return cursor{number: uint64(c.LastBlock), hash: c.LastHash}, nil //nolint:gosec // block numbers are stored from uint64 and never negative
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return cursor{}, fmt.Errorf("chain: load cursor: %w", err)
	}
	start := head
	if w.StartBlock > 0 {
		start.Number = w.StartBlock - 1
		if start.Hash, err = w.chain.BlockHash(ctx, start.Number); err != nil {
			return cursor{}, fmt.Errorf("chain: hash of start block: %w", err)
		}
	}
	err = db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := chaindb.New(tx)
		if err := q.InsertCursor(ctx, chaindb.InsertCursorParams{
			Chain: w.chain.Chain(), LastBlock: toInt64(start.Number), LastHash: start.Hash,
		}); err != nil {
			return fmt.Errorf("chain: create cursor: %w", err)
		}
		return w.putBlock(ctx, q, start)
	})
	if err != nil {
		return cursor{}, err
	}
	w.log.InfoContext(ctx, "watcher cursor created", "block", start.Number)
	return w.cursor(ctx, head)
}

// rewind finds the newest stored block that is still on the chain and
// moves the cursor back to it, orphaning pending deposits above it.
func (w *Watcher) rewind(ctx context.Context, cur cursor) (cursor, error) {
	blocks, err := chaindb.New(w.pool).RecentBlocks(ctx, chaindb.RecentBlocksParams{
		Chain: w.chain.Chain(), AtOrBelow: toInt64(cur.number),
	})
	if err != nil {
		return cur, fmt.Errorf("chain: stored blocks: %w", err)
	}
	for _, b := range blocks {
		n := uint64(b.Number) //nolint:gosec // stored from uint64
		hash, err := w.chain.BlockHash(ctx, n)
		if err != nil {
			return cur, fmt.Errorf("chain: hash of block %d: %w", n, err)
		}
		if hash != b.Hash {
			continue
		}
		fork := cursor{number: n, hash: hash}
		if err := w.moveCursorBack(ctx, fork); err != nil {
			return cur, err
		}
		w.log.WarnContext(ctx, "reorg: rewound", "from", cur.number, "to", fork.number)
		return fork, nil
	}
	return cur, fmt.Errorf("%w (cursor at %d)", ErrDeepReorg, cur.number)
}

// moveCursorBack sets the cursor to fork and orphans the pending deposits
// above it, in one transaction. Confirmed deposits above it were credited
// on blocks that no longer exist: that is logged for a human, never undone
// automatically.
func (w *Watcher) moveCursorBack(ctx context.Context, fork cursor) error {
	return db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := chaindb.New(tx)
		above := toInt64(fork.number)
		orphaned, err := q.OrphanAbove(ctx, chaindb.OrphanAboveParams{Chain: w.chain.Chain(), Number: above})
		if err != nil {
			return fmt.Errorf("chain: orphan deposits: %w", err)
		}
		credited, err := q.ConfirmedAbove(ctx, chaindb.ConfirmedAboveParams{Chain: w.chain.Chain(), Number: above})
		if err != nil {
			return fmt.Errorf("chain: confirmed deposits above fork: %w", err)
		}
		for _, id := range credited {
			obs.WatcherCreditedReorged.WithLabelValues(w.chain.Chain()).Inc()
			w.log.ErrorContext(ctx, "reorg removed a credited deposit; needs a correction entry", "deposit_id", id)
		}
		if err := q.DeleteBlocksAbove(ctx, chaindb.DeleteBlocksAboveParams{Chain: w.chain.Chain(), Number: above}); err != nil {
			return fmt.Errorf("chain: drop replaced block hashes: %w", err)
		}
		if err := q.SetCursor(ctx, chaindb.SetCursorParams{Chain: w.chain.Chain(), LastBlock: above, LastHash: fork.hash}); err != nil {
			return fmt.Errorf("chain: move cursor: %w", err)
		}
		if len(orphaned) > 0 {
			w.log.WarnContext(ctx, "reorg: orphaned pending deposits", "count", len(orphaned))
		}
		return nil
	})
}

// scan stores the transfers in blocks from..to as pending deposits and
// advances the cursor to to, in one transaction.
func (w *Watcher) scan(ctx context.Context, from, to uint64) (int, error) {
	watched, err := w.watched(ctx)
	if err != nil {
		return 0, err
	}
	var transfers []Transfer
	if len(watched) > 0 {
		if transfers, err = w.chain.ScanTransfers(ctx, from, to, watched); err != nil {
			return 0, fmt.Errorf("chain: scan %d..%d: %w", from, to, err)
		}
	}
	end, err := w.chain.BlockHash(ctx, to)
	if err != nil {
		return 0, fmt.Errorf("chain: hash of block %d: %w", to, err)
	}

	found := 0
	err = db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		found = 0
		q := chaindb.New(tx)
		for _, t := range transfers {
			customerID, ok := watched[t.To]
			if !ok || t.Amount <= 0 {
				continue // the adapter should not return these; never trust it
			}
			_, err := q.UpsertDeposit(ctx, chaindb.UpsertDepositParams{
				ID: uuid.New(), Chain: w.chain.Chain(), TxHash: t.TxHash, LogIndex: toInt64(t.LogIndex),
				BlockNumber: toInt64(t.Block.Number), BlockHash: t.Block.Hash,
				Address: t.To, CustomerID: customerID, Amount: int64(t.Amount),
			})
			switch {
			case errors.Is(err, pgx.ErrNoRows): // already known
			case err != nil:
				return fmt.Errorf("chain: store deposit %s/%d: %w", t.TxHash, t.LogIndex, err)
			default:
				found++
			}
			if err := w.putBlock(ctx, q, t.Block); err != nil {
				return err
			}
		}
		last := Block{Number: to, Hash: end}
		if err := w.putBlock(ctx, q, last); err != nil {
			return err
		}
		if err := q.SetCursor(ctx, chaindb.SetCursorParams{Chain: w.chain.Chain(), LastBlock: toInt64(to), LastHash: end}); err != nil {
			return fmt.Errorf("chain: advance cursor: %w", err)
		}
		if to > w.KeepBlocks {
			if err := q.PruneBlocks(ctx, chaindb.PruneBlocksParams{Chain: w.chain.Chain(), Below: toInt64(to - w.KeepBlocks)}); err != nil {
				return fmt.Errorf("chain: prune block hashes: %w", err)
			}
		}
		return nil
	})
	return found, err
}

func (w *Watcher) watched(ctx context.Context) (map[string]uuid.UUID, error) {
	rows, err := chaindb.New(w.pool).WatchedAddresses(ctx, w.chain.Chain())
	if err != nil {
		return nil, fmt.Errorf("chain: watched addresses: %w", err)
	}
	m := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		m[r.Address] = r.CustomerID
	}
	return m, nil
}

// provision creates the on-chain accounts of addresses still
// "provisioning" and marks them active. One address failing (say, the
// authority is out of SOL) is logged and retried next round; it does not
// stop the scan, and its deposits are still watched.
func (w *Watcher) provision(ctx context.Context) (int, error) {
	q := chaindb.New(w.pool)
	rows, err := q.ProvisioningAddresses(ctx, chaindb.ProvisioningAddressesParams{Chain: w.chain.Chain(), MaxRows: 20})
	if err != nil {
		return 0, fmt.Errorf("chain: addresses to provision: %w", err)
	}
	n := 0
	for _, r := range rows {
		if err := w.Provisioner.Provision(ctx, uint64(r.DerivationIndex), r.Address); err != nil { //nolint:gosec // from a sequence with MINVALUE 0
			if ctx.Err() != nil {
				return n, fmt.Errorf("chain: provision: %w", ctx.Err())
			}
			w.log.ErrorContext(ctx, "provision deposit address", "address", r.Address, "error", err)
			continue
		}
		done, err := q.ActivateAddress(ctx, chaindb.ActivateAddressParams{Chain: w.chain.Chain(), Address: r.Address})
		if err != nil {
			return n, fmt.Errorf("chain: activate address: %w", err)
		}
		if done == 1 {
			n++
			w.log.InfoContext(ctx, "deposit address provisioned", "address", r.Address)
		}
	}
	return n, nil
}

func (w *Watcher) putBlock(ctx context.Context, q *chaindb.Queries, b Block) error {
	if err := q.PutBlock(ctx, chaindb.PutBlockParams{Chain: w.chain.Chain(), Number: toInt64(b.Number), Hash: b.Hash}); err != nil {
		return fmt.Errorf("chain: store block hash: %w", err)
	}
	return nil
}

// confirm credits pending deposits in blocks at or below maxBlock.
func (w *Watcher) confirm(ctx context.Context, maxBlock uint64) (confirmed, orphaned int, err error) {
	ready, err := chaindb.New(w.pool).ReadyToConfirm(ctx, chaindb.ReadyToConfirmParams{
		Chain: w.chain.Chain(), MaxBlock: toInt64(maxBlock), MaxRows: 100,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("chain: deposits to confirm: %w", err)
	}
	for _, d := range ready {
		// Verify before crediting: the deposit's block must still be the
		// one on the chain. This catches a reorg that happened between
		// scan rounds even if the cursor block itself was not replaced.
		n := uint64(d.BlockNumber) //nolint:gosec // stored from uint64
		hash, err := w.chain.BlockHash(ctx, n)
		if err != nil {
			return confirmed, orphaned, fmt.Errorf("chain: hash of block %d: %w", n, err)
		}
		if hash != d.BlockHash { // exact: Solana's base58 hashes are case-sensitive
			if err := w.orphan(ctx, d, n); err != nil {
				return confirmed, orphaned, err
			}
			orphaned++
			continue
		}
		ok, err := w.credit(ctx, d)
		if err != nil {
			return confirmed, orphaned, err
		}
		if ok {
			confirmed++
		}
	}
	return confirmed, orphaned, nil
}

// orphan marks a deposit whose block was replaced and rewinds the cursor
// to just before that block, so the range is scanned again.
func (w *Watcher) orphan(ctx context.Context, d chaindb.Deposit, n uint64) error {
	w.log.WarnContext(ctx, "deposit block replaced before confirmation; orphaned", "deposit_id", d.ID, "block", n)
	if _, err := chaindb.New(w.pool).OrphanDeposit(ctx, d.ID); err != nil {
		return fmt.Errorf("chain: orphan deposit: %w", err)
	}
	if n == 0 {
		return nil
	}
	prev, err := w.chain.BlockHash(ctx, n-1)
	if err != nil {
		return fmt.Errorf("chain: hash of block %d: %w", n-1, err)
	}
	return w.moveCursorBack(ctx, cursor{number: n - 1, hash: prev})
}

// depositEvent is the outbox payload of deposit.confirmed.
type depositEvent struct {
	DepositID  uuid.UUID `json:"deposit_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Chain      string    `json:"chain"`
	Amount     string    `json:"amount"`
}

// credit confirms d and posts its deposit entry in one transaction. It
// reports false if another watcher confirmed it first.
func (w *Watcher) credit(ctx context.Context, d chaindb.Deposit) (bool, error) {
	accts, err := ledger.FindCustomerAccounts(ctx, w.pool, d.CustomerID)
	if err != nil {
		return false, fmt.Errorf("chain: accounts of %s: %w", d.CustomerID, err)
	}
	amount := ledger.Amount(d.Amount)
	done := false
	err = db.WithTx(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		n, err := chaindb.New(tx).ConfirmDeposit(ctx, d.ID)
		if err != nil {
			return fmt.Errorf("chain: confirm deposit: %w", err)
		}
		if done = n == 1; !done {
			return nil
		}
		e := ledger.Deposit(d.Chain, d.TxHash, uint64(d.LogIndex), d.ID, w.house, accts, amount) //nolint:gosec // stored from uint64
		if _, err := ledger.PostEntry(ctx, tx, e); err != nil {
			return fmt.Errorf("chain: post deposit: %w", err)
		}
		return outbox.Write(ctx, tx, "deposit.confirmed", d.ID, depositEvent{
			DepositID: d.ID, CustomerID: d.CustomerID, Chain: d.Chain, Amount: amount.String(),
		})
	})
	if err != nil {
		return false, err
	}
	if done {
		w.log.InfoContext(ctx, "deposit confirmed", "deposit_id", d.ID, "customer_id", d.CustomerID, "amount", amount.String())
	}
	return done, nil
}

// toInt64 converts a block number or index for storage. Chains stay far
// below 2^63; a value above it means a broken adapter, and panicking is
// better than storing a wrong number.
func toInt64(n uint64) int64 {
	if n > 1<<63-1 {
		panic(fmt.Sprintf("chain: value %d does not fit in bigint", n))
	}
	return int64(n)
}
