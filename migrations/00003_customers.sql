-- Customers and their API keys.
--
-- accounts.owner_id is not a foreign key to customers: ledger tests open
-- accounts for bare uuids, and the ledger does not need to know what a
-- customer is. customer.Create opens the accounts in the same transaction
-- as the customer row, so every customer has them.

-- +goose Up
CREATE TABLE customers (
    id          uuid        PRIMARY KEY,
    name        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT customers_name_len CHECK (length(name) BETWEEN 1 AND 200)
);

-- API keys are random 32-byte secrets, so a plain SHA-256 is enough to
-- store them safely (no salt or slow hash needed: there is nothing to
-- brute-force). The key itself is shown once, at creation.
CREATE TABLE api_keys (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id  uuid        NOT NULL REFERENCES customers (id),
    key_hash     bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,

    CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash),
    CONSTRAINT api_keys_key_hash_len CHECK (length(key_hash) = 32)
);

GRANT SELECT, INSERT ON customers, api_keys TO ferry_app;
GRANT UPDATE (revoked_at) ON api_keys TO ferry_app;

-- +goose Down
REVOKE ALL ON customers, api_keys FROM ferry_app;
DROP TABLE api_keys;
DROP TABLE customers;
