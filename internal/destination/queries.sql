-- name: InsertDestination :one
INSERT INTO destinations (id, customer_id, rail, country, fiat_currency, payment_method_type, display, form_ciphertext, key_id)
VALUES (@id, @customer_id, @rail, @country, @fiat_currency, @payment_method_type, @display, @form_ciphertext, @key_id)
RETURNING *;

-- name: GetDestination :one
SELECT * FROM destinations WHERE id = @id;

-- name: CustomerDestinations :many
SELECT * FROM destinations WHERE customer_id = @customer_id
ORDER BY created_at DESC
LIMIT @max_rows;

-- name: SetRailPaymentMethod :exec
-- Remembers the rail's saved payment method after the first settled
-- payout. A later, different id replaces it (the rail re-created it).
UPDATE destinations SET rail_payment_method_id = @rail_payment_method_id, updated_at = now()
WHERE id = @id AND rail_payment_method_id IS DISTINCT FROM @rail_payment_method_id;
