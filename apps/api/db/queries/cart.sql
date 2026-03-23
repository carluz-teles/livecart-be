-- name: CreateCart :one
INSERT INTO carts (session_id, platform_user_id, platform_handle, token, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetCartByID :one
SELECT * FROM carts WHERE id = $1;

-- name: GetCartByToken :one
SELECT * FROM carts WHERE token = $1;

-- name: GetCartBySessionAndUser :one
SELECT * FROM carts WHERE session_id = $1 AND platform_user_id = $2;

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

-- name: ListCartsBySession :many
SELECT * FROM carts WHERE session_id = $1 ORDER BY created_at;

-- name: CreateCartItem :one
INSERT INTO cart_items (cart_id, product_id, size, quantity, unit_price)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;
