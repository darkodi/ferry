-- Events: the relay's lease on outbox rows, and the consumer inbox.
-- See "Transactional outbox" and "Idempotent consumers" in docs/design.md.

-- +goose Up

-- While set and in the future, a relay has claimed the row and is
-- publishing it. A relay that dies leaves the lease to run out, and the
-- next relay publishes the row again (consumers absorb the duplicate).
ALTER TABLE outbox ADD COLUMN claimed_until timestamptz;

GRANT UPDATE (claimed_until) ON outbox TO ferry_app;

-- One row per message a consumer has handled. A redelivered message hits
-- the primary key and is acknowledged without doing the work again.
CREATE TABLE inbox (
    consumer      text        NOT NULL,
    message_id    text        NOT NULL,
    processed_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (consumer, message_id),
    CONSTRAINT inbox_consumer_len CHECK (length(consumer) BETWEEN 1 AND 100),
    CONSTRAINT inbox_message_id_len CHECK (length(message_id) BETWEEN 1 AND 200)
);

-- Append-only: the app may record and look up messages, never change them.
GRANT SELECT, INSERT ON inbox TO ferry_app;

-- +goose Down
REVOKE ALL ON inbox FROM ferry_app;
DROP TABLE inbox;
REVOKE UPDATE (claimed_until) ON outbox FROM ferry_app;
ALTER TABLE outbox DROP COLUMN claimed_until;
