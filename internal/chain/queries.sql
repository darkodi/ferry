-- ---- deposit addresses ----

-- name: NextDepositIndex :one
SELECT nextval('deposit_address_index')::bigint;

-- name: InsertDepositAddress :exec
INSERT INTO deposit_addresses (customer_id, chain, address, derivation_index, status)
VALUES (@customer_id, @chain, @address, @derivation_index, @status);

-- name: CustomerAddresses :many
SELECT * FROM deposit_addresses WHERE customer_id = @customer_id ORDER BY chain;

-- name: WatchedAddresses :many
-- Provisioning addresses are watched too: if the watcher stops after
-- creating an account but before marking it active, a deposit that lands
-- in between is still found.
SELECT address, customer_id FROM deposit_addresses
WHERE chain = @chain AND status IN ('active', 'provisioning');

-- name: ProvisioningAddresses :many
SELECT address, derivation_index FROM deposit_addresses
WHERE chain = @chain AND status = 'provisioning'
ORDER BY derivation_index
LIMIT @max_rows;

-- name: ActivateAddress :execrows
UPDATE deposit_addresses SET status = 'active', updated_at = now()
WHERE chain = @chain AND address = @address AND status = 'provisioning';

-- name: CustomersWithoutAddress :many
-- Customers created before a chain was configured.
SELECT c.id,
       coalesce((SELECT min(a.derivation_index) FROM deposit_addresses a WHERE a.customer_id = c.id), -1)::bigint AS existing_index
FROM customers c
WHERE NOT EXISTS (SELECT 1 FROM deposit_addresses a WHERE a.customer_id = c.id AND a.chain = @chain)
ORDER BY c.created_at;

-- ---- cursor and block hashes ----

-- name: GetCursor :one
SELECT * FROM chain_cursors WHERE chain = @chain;

-- name: InsertCursor :exec
INSERT INTO chain_cursors (chain, last_block, last_hash) VALUES (@chain, @last_block, @last_hash)
ON CONFLICT (chain) DO NOTHING;

-- name: SetCursor :exec
UPDATE chain_cursors SET last_block = @last_block, last_hash = @last_hash, updated_at = now()
WHERE chain = @chain;

-- name: PutBlock :exec
INSERT INTO chain_blocks (chain, number, hash) VALUES (@chain, @number, @hash)
ON CONFLICT (chain, number) DO UPDATE SET hash = EXCLUDED.hash;

-- name: RecentBlocks :many
-- Stored hashes at or below @at_or_below, newest first: candidates for the
-- last block both forks share.
SELECT number, hash FROM chain_blocks
WHERE chain = @chain AND number <= @at_or_below
ORDER BY number DESC;

-- name: DeleteBlocksAbove :exec
DELETE FROM chain_blocks WHERE chain = @chain AND number > @number;

-- name: PruneBlocks :exec
DELETE FROM chain_blocks WHERE chain = @chain AND number < @below;

-- ---- deposits ----

-- name: UpsertDeposit :one
-- Inserts a newly seen transfer. A transfer seen before is left alone,
-- unless it was orphaned by a reorg and has now been included again: then
-- it returns to pending at its new block. Returns no row when nothing
-- changed.
INSERT INTO deposits (id, chain, tx_hash, log_index, block_number, block_hash, address, customer_id, amount)
VALUES (@id, @chain, @tx_hash, @log_index, @block_number, @block_hash, @address, @customer_id, @amount)
ON CONFLICT (chain, tx_hash, log_index) DO UPDATE
SET status = 'pending', block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash, updated_at = now()
WHERE deposits.status = 'orphaned'
RETURNING id;

-- name: OrphanAbove :many
-- Pending deposits in blocks a reorg replaced.
UPDATE deposits SET status = 'orphaned', updated_at = now()
WHERE chain = @chain AND status = 'pending' AND block_number > @number
RETURNING id;

-- name: OrphanDeposit :execrows
UPDATE deposits SET status = 'orphaned', updated_at = now()
WHERE id = @id AND status = 'pending';

-- name: ConfirmedAbove :many
-- Confirmed deposits in blocks a reorg replaced: a reorg deeper than the
-- confirmation depth. Ferry already credited them; a human must decide.
SELECT id FROM deposits WHERE chain = @chain AND status = 'confirmed' AND block_number > @number;

-- name: ReadyToConfirm :many
SELECT * FROM deposits
WHERE chain = @chain AND status = 'pending' AND block_number <= @max_block
ORDER BY block_number, log_index
LIMIT @max_rows;

-- name: ConfirmDeposit :execrows
UPDATE deposits SET status = 'confirmed', updated_at = now()
WHERE id = @id AND status = 'pending';

-- name: CustomerDeposits :many
SELECT * FROM deposits WHERE customer_id = @customer_id
ORDER BY created_at DESC
LIMIT @max_rows;
