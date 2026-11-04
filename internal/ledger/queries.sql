-- Queries for internal/ledger. Run `make sqlc` after editing.

-- name: InsertEntry :one
-- Returns no row when the idempotency key already exists.
INSERT INTO journal_entries (idempotency_key, type, ref_id, reverses_entry_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id;

-- name: EntryIDByKey :one
SELECT id FROM journal_entries WHERE idempotency_key = $1;

-- name: LockAccounts :many
-- Locks rows in ascending id order: the sort runs before the row locks.
SELECT id, owner_id, kind, currency, posted
FROM accounts
WHERE id = ANY(@ids::bigint[])
ORDER BY id
FOR UPDATE;

-- name: InsertPosting :exec
INSERT INTO postings (entry_id, account_id, currency, direction, amount)
VALUES ($1, $2, $3, $4, $5);

-- name: SetPosted :exec
UPDATE accounts SET posted = $2, updated_at = now() WHERE id = $1;

-- name: InsertHouseAccount :exec
INSERT INTO accounts (kind, currency) VALUES ($1, $2)
ON CONFLICT (kind, currency) WHERE owner_id IS NULL DO NOTHING;

-- name: HouseAccountID :one
SELECT id FROM accounts WHERE owner_id IS NULL AND kind = $1 AND currency = $2;

-- name: InsertCustomerAccount :exec
INSERT INTO accounts (owner_id, kind, currency) VALUES ($1, $2, $3)
ON CONFLICT (owner_id, kind, currency) WHERE owner_id IS NOT NULL DO NOTHING;

-- name: CustomerAccountID :one
SELECT id FROM accounts WHERE owner_id = $1 AND kind = $2 AND currency = $3;

-- name: AccountBalance :one
SELECT posted FROM accounts WHERE id = $1;

-- name: UnbalancedCurrencies :many
-- Invariant: the sum of all postings per currency is zero.
SELECT currency,
       sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END)::numeric AS net
FROM postings
GROUP BY currency
HAVING sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) <> 0;

-- name: DriftedAccounts :many
-- Invariant: posted equals the sum of the account's postings on its normal side.
-- debit_kinds lists the debit-normal kinds; Go is the source of truth for it.
SELECT a.id, a.posted,
       coalesce(sum(CASE
           WHEN (p.direction = 'debit') = (a.kind = ANY(@debit_kinds::text[])) THEN p.amount
           ELSE -p.amount
       END), 0)::numeric AS from_postings
FROM accounts a
LEFT JOIN postings p ON p.account_id = a.id
GROUP BY a.id
HAVING a.posted <> coalesce(sum(CASE
           WHEN (p.direction = 'debit') = (a.kind = ANY(@debit_kinds::text[])) THEN p.amount
           ELSE -p.amount
       END), 0);

-- name: NegativeAccounts :many
SELECT id, owner_id, kind, posted FROM accounts WHERE posted < 0 ORDER BY id;
