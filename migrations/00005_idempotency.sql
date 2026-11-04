-- Client Idempotency-Key records for mutating API endpoints.
-- See "Idempotent API" in docs/design.md.

-- +goose Up
CREATE TABLE idempotency_keys (
    customer_id      uuid        NOT NULL REFERENCES customers (id),
    key              text        NOT NULL,
    -- SHA-256 of method, path and body. Same key with a different hash is
    -- a client bug and is refused.
    request_hash     bytea       NOT NULL,
    -- While set and in the future, a request with this key is running.
    -- NULL once a response is stored or the request gave up.
    locked_until     timestamptz,
    response_status  integer,
    response_body    bytea,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (customer_id, key),
    CONSTRAINT idempotency_keys_key_len CHECK (length(key) BETWEEN 1 AND 200),
    CONSTRAINT idempotency_keys_hash_len CHECK (length(request_hash) = 32)
);

GRANT SELECT, INSERT ON idempotency_keys TO ferry_app;
GRANT UPDATE (locked_until, response_status, response_body, updated_at) ON idempotency_keys TO ferry_app;

-- +goose Down
REVOKE ALL ON idempotency_keys FROM ferry_app;
DROP TABLE idempotency_keys;
