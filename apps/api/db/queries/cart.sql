-- =============================================================================
-- CARTS (belong to events, not sessions)
-- =============================================================================

-- name: CreateCart :one
INSERT INTO carts (event_id, platform_user_id, platform_handle, token, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetCartByID :one
SELECT * FROM carts WHERE id = $1;

-- name: GetCartByToken :one
SELECT * FROM carts WHERE token = $1;

-- name: GetCartByEventAndUser :one
SELECT * FROM carts WHERE event_id = $1 AND platform_user_id = $2;

-- name: UpdateCartStatus :one
UPDATE carts SET status = $2 WHERE id = $1 RETURNING *;

-- name: UpdateCartPayment :one
UPDATE carts
SET payment_status = $2, external_order_id = $3, paid_at = $4
WHERE id = $1
RETURNING *;

-- name: UpdateCartNotifyStatus :one
UPDATE carts
SET notify_status = $2, notify_error = $3, notified_at = $4
WHERE id = $1
RETURNING *;

-- name: ListCartsByEvent :many
SELECT * FROM carts WHERE event_id = $1 ORDER BY created_at;

-- name: CreateCartItem :one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpsertCartItem :one
-- Adds quantity to existing cart item or creates new one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
VALUES ($1, $2, $3, $4)
ON CONFLICT (cart_id, product_id)
DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity
RETURNING *;

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- name: FinalizeCartsByEvent :exec
-- Updates all pending carts in an event to checkout status
UPDATE carts
SET status = 'checkout'
WHERE event_id = $1 AND status = 'pending';

-- name: CountCartsByEvent :one
SELECT COUNT(*)::int as count FROM carts WHERE event_id = $1 AND status = 'pending';

-- name: UpdateCartItem :one
UPDATE cart_items
SET quantity = $2
WHERE id = $1
RETURNING *;

-- name: DeleteCartItem :exec
DELETE FROM cart_items WHERE id = $1;

-- name: GetCartItem :one
SELECT * FROM cart_items WHERE id = $1;
