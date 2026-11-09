-- Verified rail webhooks, kept as received.
-- See "Rail integration (Noah)" in docs/design.md.

-- +goose Up

-- One row per (rail, transaction, event version). A webhook is stored
-- here before Ferry answers 2xx, so a delivery is never lost; a
-- redelivery hits the unique key and changes nothing. The payload is the
-- verified body, for audit; money only moves after Ferry re-fetches the
-- transaction from the rail.
CREATE TABLE rail_events (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rail           text        NOT NULL,
    event_type     text        NOT NULL,
    rail_tx_id     text        NOT NULL,
    event_version  bigint      NOT NULL,
    occurred       timestamptz,
    payload        jsonb       NOT NULL,
    -- The payout the event was matched to, if any (unknown transactions
    -- are stored too).
    payout_id      uuid REFERENCES payouts (id),
    received_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT rail_events_key UNIQUE (rail, rail_tx_id, event_version),
    CONSTRAINT rail_events_rail_valid CHECK (rail ~ '^[a-z0-9-]{1,32}$'),
    CONSTRAINT rail_events_type_len CHECK (length(event_type) BETWEEN 1 AND 64),
    CONSTRAINT rail_events_tx_len CHECK (length(rail_tx_id) BETWEEN 1 AND 100),
    CONSTRAINT rail_events_version_positive CHECK (event_version > 0)
);

-- Append-only: the app records and reads events, never changes them.
GRANT SELECT, INSERT ON rail_events TO ferry_app;

-- +goose Down
REVOKE ALL ON rail_events FROM ferry_app;
DROP TABLE rail_events;
