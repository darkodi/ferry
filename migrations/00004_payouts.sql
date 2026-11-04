-- Payouts, their quotes, rail attempts and the transactional outbox.
-- See "Workflow records" and "Payout state machine" in docs/design.md.

-- +goose Up

-- One row per payout. status follows the state machine; every transition
-- is UPDATE ... WHERE id = $1 AND status = $2 AND version = $3.
CREATE TABLE payouts (
    id                uuid        PRIMARY KEY,
    customer_id       uuid        NOT NULL REFERENCES customers (id),
    -- The client's Idempotency-Key. Unique per customer, so a retried
    -- create can never make a second payout.
    idempotency_key   text        NOT NULL,
    fiat_amount       bigint      NOT NULL,  -- minor units (cents); metadata, never posted
    fiat_currency     text        NOT NULL,
    destination       text        NOT NULL,  -- the rail's id for the bank account
    status            text        NOT NULL DEFAULT 'created',
    -- Set when a rail submit ended with an unknown outcome (timeout, 5xx).
    -- The resolver (M3) clears it.
    needs_resolution  boolean     NOT NULL DEFAULT false,
    failure_reason    text,
    version           integer     NOT NULL DEFAULT 1,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT payouts_customer_key_key UNIQUE (customer_id, idempotency_key),
    CONSTRAINT payouts_idempotency_key_len CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    CONSTRAINT payouts_fiat_amount_positive CHECK (fiat_amount > 0),
    CONSTRAINT payouts_fiat_currency_valid CHECK (fiat_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payouts_destination_len CHECK (length(destination) BETWEEN 1 AND 200),
    CONSTRAINT payouts_status_valid CHECK (status IN
        ('created', 'quoted', 'held', 'submitted', 'settled', 'reversed', 'failed', 'expired'))
);

CREATE INDEX payouts_customer_idx ON payouts (customer_id, created_at);

-- The rail's price for a payout. One per payout in v1.
CREATE TABLE quotes (
    id                 uuid           PRIMARY KEY,
    payout_id          uuid           NOT NULL REFERENCES payouts (id),
    rail               text           NOT NULL,
    rail_quote_id      text           NOT NULL,
    fiat_amount        bigint         NOT NULL,
    fiat_currency      text           NOT NULL,
    -- The most USDC the rail may take (Noah: CryptoAuthorizedAmount).
    -- This is what the hold reserves.
    crypto_authorized  numeric(38, 0) NOT NULL,
    expires_at         timestamptz    NOT NULL,
    created_at         timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT quotes_payout_id_key UNIQUE (payout_id),
    CONSTRAINT quotes_crypto_authorized_positive_int64
        CHECK (crypto_authorized > 0 AND crypto_authorized <= 9223372036854775807)
);

-- One row per submit attempt. The nonce is generated and stored before the
-- rail is called, and every retry of the attempt reuses it.
CREATE TABLE rail_transactions (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payout_id           uuid        NOT NULL REFERENCES payouts (id),
    attempt             integer     NOT NULL,
    nonce               text        NOT NULL,
    external_id         text        NOT NULL,  -- = payout id
    rail_tx_id          text,                  -- known once the rail accepts
    -- created: nonce stored, rail outcome not yet known.
    status              text        NOT NULL DEFAULT 'created',
    sub_status          text,
    last_event_version  bigint      NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT rail_transactions_nonce_key UNIQUE (nonce),
    CONSTRAINT rail_transactions_attempt_key UNIQUE (payout_id, attempt),
    CONSTRAINT rail_transactions_nonce_len CHECK (length(nonce) BETWEEN 1 AND 36),
    CONSTRAINT rail_transactions_status_valid
        CHECK (status IN ('created', 'pending', 'settled', 'failed'))
);

-- Events written in the same transaction as the state change they
-- describe. The relay (M3) publishes rows with published_at IS NULL.
CREATE TABLE outbox (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    topic         text        NOT NULL,
    aggregate_id  uuid        NOT NULL,
    payload       jsonb       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz
);

CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- Least privilege: the app may create rows and change only the columns a
-- workflow step changes. No DELETE anywhere.
GRANT SELECT, INSERT ON payouts, quotes, rail_transactions, outbox TO ferry_app;
GRANT UPDATE (status, needs_resolution, failure_reason, version, updated_at) ON payouts TO ferry_app;
GRANT UPDATE (rail_tx_id, status, sub_status, last_event_version, updated_at) ON rail_transactions TO ferry_app;
GRANT UPDATE (published_at) ON outbox TO ferry_app;

-- +goose Down
REVOKE ALL ON payouts, quotes, rail_transactions, outbox FROM ferry_app;
DROP TABLE outbox;
DROP TABLE rail_transactions;
DROP TABLE quotes;
DROP TABLE payouts;
