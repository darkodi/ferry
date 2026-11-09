-- name: InsertPayout :one
-- Returns no row when this customer already used the idempotency key.
INSERT INTO payouts (id, customer_id, idempotency_key, fiat_amount, fiat_currency, destination, destination_id)
VALUES (@id, @customer_id, @idempotency_key, @fiat_amount, @fiat_currency, @destination, @destination_id)
ON CONFLICT (customer_id, idempotency_key) DO NOTHING
RETURNING id;

-- name: PayoutByKey :one
SELECT id FROM payouts WHERE customer_id = @customer_id AND idempotency_key = @idempotency_key;

-- name: GetPayout :one
SELECT * FROM payouts WHERE id = @id;

-- name: Transition :execrows
-- Moves a payout from one state to the next. Zero rows means another
-- request changed it first; the caller re-reads and stops.
UPDATE payouts
SET status = @to_status, failure_reason = @failure_reason, needs_resolution = false,
    version = version + 1, updated_at = now()
WHERE id = @id AND status = @from_status AND version = @version;

-- name: MarkNeedsResolution :execrows
UPDATE payouts
SET needs_resolution = true, version = version + 1, updated_at = now()
WHERE id = @id AND status = @status AND version = @version;

-- name: InsertQuote :exec
INSERT INTO quotes (id, payout_id, rail, rail_quote_id, fiat_amount, fiat_currency, crypto_authorized, expires_at)
VALUES (@id, @payout_id, @rail, @rail_quote_id, @fiat_amount, @fiat_currency, @crypto_authorized, @expires_at);

-- name: GetQuote :one
SELECT * FROM quotes WHERE payout_id = @payout_id;

-- name: LatestRailTx :one
SELECT * FROM rail_transactions WHERE payout_id = @payout_id ORDER BY attempt DESC LIMIT 1;

-- name: InsertRailTx :one
-- Returns no row if a concurrent submit already created this attempt.
INSERT INTO rail_transactions (payout_id, attempt, nonce, external_id)
VALUES (@payout_id, @attempt, @nonce, @external_id)
ON CONFLICT (payout_id, attempt) DO NOTHING
RETURNING *;

-- name: SetRailTxStatus :exec
-- Records the submit result: accepted (pending, with the rail's id) or
-- rejected (failed). Later changes go through UpdateRailTx.
UPDATE rail_transactions
SET rail_tx_id = @rail_tx_id, status = @status, updated_at = now()
WHERE id = @id;

-- name: UpdateRailTx :execrows
-- Applies a rail event only if it is newer than the last one applied.
UPDATE rail_transactions
SET rail_tx_id = @rail_tx_id, status = @status, sub_status = @sub_status,
    last_event_version = @event_version, updated_at = now()
WHERE id = @id AND last_event_version < @event_version;

-- name: UpdateRailSubStatus :execrows
-- Records a new sub-status reported at the version already applied (a
-- review moving on while the status stays pending). Moves no money.
UPDATE rail_transactions
SET sub_status = @sub_status, updated_at = now()
WHERE id = @id AND last_event_version = @event_version AND sub_status IS DISTINCT FROM @sub_status;

-- name: HeldMismatches :many
-- Customers whose held balance differs from the authorized amounts of
-- their payouts in held or submitted.
SELECT a.owner_id::uuid AS customer_id, a.posted AS held,
       coalesce(sum(q.crypto_authorized) FILTER (WHERE p.status IN ('held', 'submitted')), 0)::numeric AS expected
FROM accounts a
JOIN payouts p ON p.customer_id = a.owner_id
LEFT JOIN quotes q ON q.payout_id = p.id
WHERE a.kind = 'held'
GROUP BY a.owner_id, a.posted
HAVING a.posted <> coalesce(sum(q.crypto_authorized) FILTER (WHERE p.status IN ('held', 'submitted')), 0);

-- name: SettleEntryCounts :many
-- Settled or reversed payouts that do not have exactly one settle entry.
SELECT p.id, count(e.id) AS entries
FROM payouts p
LEFT JOIN journal_entries e ON e.idempotency_key = 'payout:' || p.id::text || ':settle'
WHERE p.status IN ('settled', 'reversed')
GROUP BY p.id
HAVING count(e.id) <> 1;

-- name: StaleInFlight :many
-- Payouts in @status untouched for at least @stale_secs seconds, oldest
-- first. The resolver resubmits stale held payouts (lost event or unknown
-- submit outcome) and re-fetches stale submitted ones (missed webhook).
-- The cut-off uses the database clock, the same clock as updated_at.
SELECT id FROM payouts
WHERE status = @status
  AND updated_at <= now() - make_interval(secs => @stale_secs::int)
ORDER BY updated_at
LIMIT @max_rows;

-- name: PayoutForRailTx :one
-- The payout a rail transaction belongs to, for the named rail.
SELECT r.payout_id FROM rail_transactions r
JOIN quotes q ON q.payout_id = r.payout_id
WHERE r.rail_tx_id = @rail_tx_id AND q.rail = @rail
LIMIT 1;

-- name: MaxRailEventVersion :one
SELECT coalesce(max(event_version), 0)::bigint FROM rail_events
WHERE rail = @rail AND rail_tx_id = @rail_tx_id;

-- name: InsertRailEvent :one
-- Returns no row for a redelivered event (same rail, transaction, version).
INSERT INTO rail_events (rail, event_type, rail_tx_id, event_version, occurred, payload, payout_id)
VALUES (@rail, @event_type, @rail_tx_id, @event_version, @occurred, @payload, @payout_id)
ON CONFLICT (rail, rail_tx_id, event_version) DO NOTHING
RETURNING id;
