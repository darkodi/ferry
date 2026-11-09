-- Payout destinations: a customer's saved bank accounts.
-- See "Rail integration (Noah)" in docs/design.md.

-- +goose Up

-- The bank details (the rail's payment form) are encrypted by the
-- application with AES-256-GCM before they reach the database; only
-- display holds a masked summary. The database never sees them in clear.
CREATE TABLE destinations (
    id                      uuid        PRIMARY KEY,
    customer_id             uuid        NOT NULL REFERENCES customers (id),
    -- The rail whose form shape this destination follows, e.g. 'noah'.
    rail                    text        NOT NULL,
    country                 text        NOT NULL,  -- ISO 3166-1 alpha-2
    fiat_currency           text        NOT NULL,
    payment_method_type     text        NOT NULL,  -- e.g. BankAch, BankFedwire
    display                 text        NOT NULL,  -- e.g. "BankAch ****5678"
    -- nonce || ciphertext || tag; associated data binds it to this row's id.
    form_ciphertext         bytea       NOT NULL,
    key_id                  text        NOT NULL,  -- which data key encrypted it
    -- The rail's saved payment method, learned from the first settled
    -- payout and reused afterwards.
    rail_payment_method_id  text,
    status                  text        NOT NULL DEFAULT 'active',
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT destinations_rail_valid CHECK (rail ~ '^[a-z0-9-]{1,32}$'),
    CONSTRAINT destinations_country_valid CHECK (country ~ '^[A-Z]{2}$'),
    CONSTRAINT destinations_fiat_currency_valid CHECK (fiat_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT destinations_method_valid CHECK (payment_method_type ~ '^[A-Za-z]{2,40}$'),
    CONSTRAINT destinations_display_len CHECK (length(display) BETWEEN 1 AND 80),
    CONSTRAINT destinations_form_len CHECK (length(form_ciphertext) BETWEEN 29 AND 16384),
    CONSTRAINT destinations_pm_len CHECK (length(rail_payment_method_id) BETWEEN 1 AND 150),
    CONSTRAINT destinations_status_valid CHECK (status IN ('active', 'disabled'))
);

CREATE INDEX destinations_customer_idx ON destinations (customer_id, created_at);

-- New payouts name a destination; older ones keep only the free-text
-- destination column they were created with.
ALTER TABLE payouts ADD COLUMN destination_id uuid REFERENCES destinations (id);

GRANT SELECT, INSERT ON destinations TO ferry_app;
GRANT UPDATE (rail_payment_method_id, status, updated_at) ON destinations TO ferry_app;

-- +goose Down
REVOKE ALL ON destinations FROM ferry_app;
ALTER TABLE payouts DROP COLUMN destination_id;
DROP TABLE destinations;
