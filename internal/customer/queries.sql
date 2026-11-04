-- name: InsertCustomer :exec
INSERT INTO customers (id, name) VALUES (@id, @name);

-- name: GetCustomer :one
SELECT id, name, created_at FROM customers WHERE id = @id;

-- name: InsertAPIKey :exec
INSERT INTO api_keys (customer_id, key_hash) VALUES (@customer_id, @key_hash);

-- name: CustomerByKeyHash :one
SELECT customer_id FROM api_keys WHERE key_hash = @key_hash AND revoked_at IS NULL;
