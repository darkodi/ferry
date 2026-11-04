-- name: ClaimNew :one
-- Returns no row if the key already exists.
INSERT INTO idempotency_keys (customer_id, key, request_hash, locked_until)
VALUES (@customer_id, @key, @request_hash, @locked_until)
ON CONFLICT (customer_id, key) DO NOTHING
RETURNING key;

-- name: GetForUpdate :one
SELECT * FROM idempotency_keys WHERE customer_id = @customer_id AND key = @key FOR UPDATE;

-- name: Relock :exec
UPDATE idempotency_keys SET locked_until = @locked_until, updated_at = now()
WHERE customer_id = @customer_id AND key = @key;

-- name: Complete :exec
UPDATE idempotency_keys
SET response_status = @response_status, response_body = @response_body, locked_until = NULL, updated_at = now()
WHERE customer_id = @customer_id AND key = @key;

-- name: Unlock :exec
UPDATE idempotency_keys SET locked_until = NULL, updated_at = now()
WHERE customer_id = @customer_id AND key = @key AND response_status IS NULL;
