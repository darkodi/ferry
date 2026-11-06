-- name: Claim :many
-- Leases up to @max_rows unpublished rows that no other relay holds.
-- SKIP LOCKED lets several relays claim different rows at the same time.
UPDATE outbox
SET claimed_until = @claimed_until::timestamptz
WHERE id IN (
    SELECT o.id FROM outbox o
    WHERE o.published_at IS NULL
      AND (o.claimed_until IS NULL OR o.claimed_until < @now::timestamptz)
    ORDER BY o.id
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING id, topic, aggregate_id, payload;

-- name: MarkPublished :exec
UPDATE outbox SET published_at = @published_at::timestamptz, claimed_until = NULL
WHERE id = ANY(@ids::bigint[]) AND published_at IS NULL;
