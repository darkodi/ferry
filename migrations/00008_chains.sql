-- Deposit addresses, detected deposits and chain watcher state.
-- See "Blockchain integration" and "Deposit (USDC in)" in docs/design.md.

-- +goose Up

-- Each customer gets one derivation index, used on every chain. Gaps
-- (a rolled-back customer) are harmless.
CREATE SEQUENCE deposit_address_index AS bigint MINVALUE 0 START 0;

CREATE TABLE deposit_addresses (
    customer_id       uuid        NOT NULL REFERENCES customers (id),
    chain             text        NOT NULL,
    address           text        NOT NULL,
    derivation_index  bigint      NOT NULL,
    -- 'provisioning' while an on-chain account must still be created
    -- (Solana); 'active' once deposits can arrive.
    status            text        NOT NULL DEFAULT 'active',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (customer_id, chain),
    CONSTRAINT deposit_addresses_address_key UNIQUE (chain, address),
    CONSTRAINT deposit_addresses_index_key UNIQUE (chain, derivation_index),
    CONSTRAINT deposit_addresses_chain_valid CHECK (chain ~ '^[a-z0-9-]{1,32}$'),
    CONSTRAINT deposit_addresses_status_valid CHECK (status IN ('provisioning', 'active'))
);

-- One row per USDC transfer to a deposit address. Credited only when
-- confirmed; a pending deposit whose block is reorged out is orphaned (and
-- comes back to pending if the same transfer is included again).
CREATE TABLE deposits (
    id            uuid           PRIMARY KEY,
    chain         text           NOT NULL,
    tx_hash       text           NOT NULL,
    log_index     bigint         NOT NULL,
    block_number  bigint         NOT NULL,
    block_hash    text           NOT NULL,
    address       text           NOT NULL,
    customer_id   uuid           NOT NULL REFERENCES customers (id),
    amount        numeric(38, 0) NOT NULL,
    status        text           NOT NULL DEFAULT 'pending',
    created_at    timestamptz    NOT NULL DEFAULT now(),
    updated_at    timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT deposits_chain_tx_log_key UNIQUE (chain, tx_hash, log_index),
    CONSTRAINT deposits_amount_positive CHECK (amount > 0 AND amount <= 9223372036854775807),
    CONSTRAINT deposits_status_valid CHECK (status IN ('pending', 'confirmed', 'orphaned'))
);

CREATE INDEX deposits_pending_idx ON deposits (chain, block_number) WHERE status = 'pending';
CREATE INDEX deposits_customer_idx ON deposits (customer_id, created_at);

-- How far each chain has been scanned.
CREATE TABLE chain_cursors (
    chain        text        PRIMARY KEY,
    last_block   bigint      NOT NULL,
    last_hash    text        NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Hashes of recently scanned blocks, to find where a reorg forked.
-- Pruned to the most recent few hundred per chain.
CREATE TABLE chain_blocks (
    chain   text   NOT NULL,
    number  bigint NOT NULL,
    hash    text   NOT NULL,
    PRIMARY KEY (chain, number)
);

GRANT USAGE ON SEQUENCE deposit_address_index TO ferry_app;
GRANT SELECT, INSERT ON deposit_addresses, deposits, chain_cursors, chain_blocks TO ferry_app;
GRANT UPDATE (status, updated_at) ON deposit_addresses TO ferry_app;
-- block_number and block_hash change only when an orphaned deposit is
-- included again after a reorg.
GRANT UPDATE (status, block_number, block_hash, updated_at) ON deposits TO ferry_app;
GRANT UPDATE (last_block, last_hash, updated_at) ON chain_cursors TO ferry_app;
-- Rewinds replace block hashes; pruning drops old ones. Deposits are never deleted.
GRANT UPDATE (hash), DELETE ON chain_blocks TO ferry_app;

-- +goose Down
REVOKE ALL ON chain_blocks, chain_cursors, deposits, deposit_addresses FROM ferry_app;
REVOKE ALL ON SEQUENCE deposit_address_index FROM ferry_app;
DROP TABLE chain_blocks;
DROP TABLE chain_cursors;
DROP TABLE deposits;
DROP TABLE deposit_addresses;
DROP SEQUENCE deposit_address_index;
