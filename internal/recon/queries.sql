-- ---- runs and breaks ----

-- name: InsertRun :one
INSERT INTO recon_runs (window_from, window_to, checks) VALUES (@window_from, @window_to, '{}')
RETURNING id;

-- name: FinishRun :exec
UPDATE recon_runs SET finished_at = now(), checks = @checks, failed = @failed, breaks_seen = @breaks_seen
WHERE id = @id;

-- name: UpsertBreak :one
-- Records a disagreement seen in this run. A live break with the same key
-- is updated; if it was resolved and its detail is unchanged it stays
-- resolved, otherwise it reopens. Returns whether the row is new.
INSERT INTO recon_breaks (id, check_name, kind, subject, detail, last_run_id)
VALUES (@id, @check_name, @kind, @subject, @detail, @run_id)
ON CONFLICT (check_name, kind, subject) WHERE cleared_at IS NULL DO UPDATE
SET detail = EXCLUDED.detail,
    last_seen = now(),
    seen_count = recon_breaks.seen_count + 1,
    last_run_id = EXCLUDED.last_run_id,
    resolved_at = CASE WHEN recon_breaks.detail = EXCLUDED.detail THEN recon_breaks.resolved_at END,
    resolved_reason = CASE WHEN recon_breaks.detail = EXCLUDED.detail THEN recon_breaks.resolved_reason END
RETURNING id, (xmax = 0) AS inserted;

-- name: ClearUnseen :execrows
-- Clears the live breaks of a check that this run (which completed that
-- check) did not see again.
UPDATE recon_breaks SET cleared_at = now()
WHERE check_name = @check_name AND left(subject, length(@prefix::text)) = @prefix::text
  AND cleared_at IS NULL AND last_run_id <> @run_id;

-- name: InsertMatch :exec
INSERT INTO recon_matches (run_id, check_name, ferry_ref, external_ref)
VALUES (@run_id, @check_name, @ferry_ref, @external_ref)
ON CONFLICT DO NOTHING;

-- name: ListBreaks :many
-- Live breaks (not cleared), unresolved first, oldest first.
SELECT * FROM recon_breaks
WHERE cleared_at IS NULL AND (@include_resolved::bool OR resolved_at IS NULL)
ORDER BY resolved_at IS NOT NULL, first_seen
LIMIT @max_rows;

-- name: ResolveBreak :execrows
UPDATE recon_breaks SET resolved_at = now(), resolved_reason = @reason
WHERE id = @id AND cleared_at IS NULL AND resolved_at IS NULL;

-- name: OpenBreakCount :one
SELECT count(*) FROM recon_breaks WHERE cleared_at IS NULL AND resolved_at IS NULL;

-- ---- chain ----

-- name: ChainAddresses :many
SELECT address FROM deposit_addresses WHERE chain = @chain ORDER BY address;

-- name: ChainCursor :one
SELECT last_block FROM chain_cursors WHERE chain = @chain;

-- name: RecordedDeposits :many
-- What Ferry recorded per address: pending and confirmed deposits in
-- blocks up to @max_block (orphaned ones are not on the chain).
SELECT address, coalesce(sum(amount), 0)::bigint AS total
FROM deposits
WHERE chain = @chain AND status IN ('pending', 'confirmed') AND block_number <= @max_block
GROUP BY address;

-- name: ConfirmedDepositTotal :one
SELECT coalesce(sum(amount), 0)::bigint FROM deposits WHERE chain = @chain AND status = 'confirmed';

-- name: LedgerDepositTotal :one
-- What the ledger credited for a chain's deposits: deposit entries keyed
-- deposit:<chain>:..., summed over their customer credit.
SELECT coalesce(sum(p.amount), 0)::bigint
FROM journal_entries e
JOIN postings p ON p.entry_id = e.id AND p.direction = 'credit'
WHERE e.type = 'deposit' AND left(e.idempotency_key, length(@prefix::text)) = @prefix::text;

-- ---- rail ----

-- name: RailTxsInWindow :many
-- Ferry's rail attempts for one rail created in the window, with their
-- payout's state.
SELECT r.id, r.payout_id, r.rail_tx_id, r.status AS rail_status, p.status AS payout_status, r.created_at
FROM rail_transactions r
JOIN quotes q ON q.payout_id = r.payout_id
JOIN payouts p ON p.id = r.payout_id
WHERE q.rail = @rail AND r.created_at >= @from_time AND r.created_at < @to_time
ORDER BY r.created_at;

-- name: RailTxByRailID :one
SELECT r.id, r.payout_id, r.rail_tx_id, r.status AS rail_status, p.status AS payout_status
FROM rail_transactions r
JOIN quotes q ON q.payout_id = r.payout_id
JOIN payouts p ON p.id = r.payout_id
WHERE q.rail = @rail AND r.rail_tx_id = @rail_tx_id
LIMIT 1;

-- name: RailTxByPayout :one
SELECT r.id, r.payout_id, r.rail_tx_id, r.status AS rail_status, p.status AS payout_status
FROM rail_transactions r
JOIN quotes q ON q.payout_id = r.payout_id
JOIN payouts p ON p.id = r.payout_id
WHERE q.rail = @rail AND r.payout_id = @payout_id
ORDER BY r.attempt DESC
LIMIT 1;

-- name: EntryAmount :one
-- The amount an entry (by idempotency key) posts to accounts of one kind
-- on one side, e.g. what a settle credited to noah_prefunded; 0 if there
-- is no such entry.
SELECT coalesce(sum(p.amount), 0)::bigint
FROM journal_entries e
JOIN postings p ON p.entry_id = e.id AND p.direction = @direction
JOIN accounts a ON a.id = p.account_id AND a.kind = @kind
WHERE e.idempotency_key = @idempotency_key;
