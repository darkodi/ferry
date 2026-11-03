-- Ledger core: accounts (with their cached balance), journal entries and postings.
-- See the Ledger section of docs/design.md for the model and its rules.

-- +goose Up

-- Accounts. One per (owner, kind, currency). `posted` is the balance on the
-- account's normal side (credit-normal: credits - debits; debit-normal:
-- debits - credits), updated in the same transaction as the postings.
CREATE TABLE accounts (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    uuid,
    kind        text           NOT NULL,
    currency    text           NOT NULL,
    posted      numeric(38, 0) NOT NULL DEFAULT 0,
    created_at  timestamptz    NOT NULL DEFAULT now(),
    updated_at  timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT accounts_kind_valid
        CHECK (kind IN ('available', 'held', 'chain_omnibus', 'noah_prefunded', 'fees')),
    -- Customer kinds have an owner; house kinds never do.
    CONSTRAINT accounts_owner_matches_kind
        CHECK ((kind IN ('available', 'held')) = (owner_id IS NOT NULL)),
    CONSTRAINT accounts_currency_valid
        CHECK (currency = 'USDC'),
    -- Customer accounts can never go negative. House accounts may.
    CONSTRAINT accounts_customer_not_negative
        CHECK (owner_id IS NULL OR posted >= 0),
    CONSTRAINT accounts_posted_in_int64_range
        CHECK (posted BETWEEN -9223372036854775808 AND 9223372036854775807),
    -- Target of the composite FK from postings.
    CONSTRAINT accounts_id_currency_key UNIQUE (id, currency)
);

CREATE UNIQUE INDEX accounts_customer_key ON accounts (owner_id, kind, currency) WHERE owner_id IS NOT NULL;
CREATE UNIQUE INDEX accounts_house_key    ON accounts (kind, currency)           WHERE owner_id IS NULL;

-- Journal entries. One business event each.
CREATE TABLE journal_entries (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key    text        NOT NULL,
    type               text        NOT NULL,
    ref_id             uuid,
    reverses_entry_id  bigint      REFERENCES journal_entries (id),
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT journal_entries_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT journal_entries_idempotency_key_len
        CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    CONSTRAINT journal_entries_type_valid
        CHECK (type IN ('deposit', 'treasury_topup', 'hold', 'settle', 'release', 'bank_return', 'correction')),
    -- An entry can be mirrored by at most one correction.
    CONSTRAINT journal_entries_reverses_entry_id_key UNIQUE (reverses_entry_id),
    CONSTRAINT journal_entries_only_corrections_reverse
        CHECK (reverses_entry_id IS NULL OR type = 'correction'),
    CONSTRAINT journal_entries_not_self_reversing
        CHECK (reverses_entry_id IS NULL OR reverses_entry_id <> id)
);

-- Postings. The lines of an entry; amount is always positive, direction carries the sign.
CREATE TABLE postings (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id    bigint         NOT NULL REFERENCES journal_entries (id),
    account_id  bigint         NOT NULL,
    currency    text           NOT NULL,
    direction   text           NOT NULL,
    amount      numeric(38, 0) NOT NULL,

    CONSTRAINT postings_account_currency_fkey
        FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency),
    CONSTRAINT postings_direction_valid
        CHECK (direction IN ('debit', 'credit')),
    CONSTRAINT postings_amount_positive_int64
        CHECK (amount > 0 AND amount <= 9223372036854775807)
);

CREATE INDEX postings_entry_id_idx   ON postings (entry_id);
CREATE INDEX postings_account_id_idx ON postings (account_id, id);

-- Balanced entries. Checked at COMMIT (deferred), once all postings of an
-- entry are in: an entry needs at least two postings, and per currency
-- debits must equal credits. Fires on new entries (so an entry with no
-- postings is rejected) and on new postings (so lines added to an old entry
-- later are re-checked).
-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_entry_id bigint;
    v_lines    bigint;
    v_currency text;
BEGIN
    IF TG_TABLE_NAME = 'journal_entries' THEN
        v_entry_id := NEW.id;
    ELSE
        v_entry_id := NEW.entry_id;
    END IF;

    SELECT count(*) INTO v_lines FROM postings WHERE entry_id = v_entry_id;
    IF v_lines < 2 THEN
        RAISE EXCEPTION 'journal entry % has % posting(s); at least 2 required', v_entry_id, v_lines
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT currency INTO v_currency
    FROM postings
    WHERE entry_id = v_entry_id
    GROUP BY currency
    HAVING sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) <> 0
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'journal entry % is unbalanced in %', v_entry_id, v_currency
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER journal_entries_balanced
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_balanced();

CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_balanced();

-- Append-only. Blocks UPDATE, DELETE and TRUNCATE on entries and postings for
-- every role, including the table owner. Revoked grants (00002) are the
-- first line of defence; these triggers are the second.
-- +goose StatementBegin
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % rejected', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER journal_entries_append_only
    BEFORE UPDATE OR DELETE ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER journal_entries_no_truncate
    BEFORE TRUNCATE ON journal_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER postings_append_only
    BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER postings_no_truncate
    BEFORE TRUNCATE ON postings
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- +goose Down
DROP TABLE postings;
DROP TABLE journal_entries;
DROP TABLE accounts;
DROP FUNCTION ledger_reject_mutation();
DROP FUNCTION ledger_check_entry_balanced();
