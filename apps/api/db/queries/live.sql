-- name: CreateLiveSession :one
INSERT INTO live_sessions (store_id, platform, platform_live_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetLiveSessionByID :one
SELECT * FROM live_sessions WHERE id = $1;

-- name: GetActiveLiveSession :one
SELECT * FROM live_sessions
WHERE store_id = $1 AND platform_live_id = $2 AND status = 'active';

-- name: EndLiveSession :one
UPDATE live_sessions
SET status = 'ended', ended_at = now(), total_comments = $2, total_orders = $3
WHERE id = $1
RETURNING *;

-- name: ListLiveSessionsByStore :many
SELECT * FROM live_sessions WHERE store_id = $1 ORDER BY started_at DESC;

-- name: UpsertDetectedOrder :one
INSERT INTO detected_orders (session_id, platform_user_id, platform_handle, comment_text, product_id, quantity)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, platform_user_id, product_id)
DO UPDATE SET quantity = detected_orders.quantity + EXCLUDED.quantity, comment_text = EXCLUDED.comment_text
RETURNING *;

-- name: CancelDetectedOrder :exec
UPDATE detected_orders
SET cancelled = true
WHERE session_id = $1 AND platform_user_id = $2 AND product_id = $3;

-- name: ListDetectedOrdersBySession :many
SELECT * FROM detected_orders
WHERE session_id = $1 AND cancelled = false
ORDER BY detected_at;

-- name: ListDistinctUsersBySession :many
SELECT DISTINCT platform_user_id, platform_handle
FROM detected_orders
WHERE session_id = $1 AND cancelled = false;

-- name: ListDetectedOrdersByUser :many
SELECT d.*, p.name AS product_name, p.price AS product_price, p.sizes AS product_sizes
FROM detected_orders d
JOIN products p ON p.id = d.product_id
WHERE d.session_id = $1 AND d.platform_user_id = $2 AND d.cancelled = false;

-- name: UpdateDetectedOrderCartID :exec
UPDATE detected_orders SET cart_id = $2
WHERE session_id = $1 AND platform_user_id = $3 AND cancelled = false;
