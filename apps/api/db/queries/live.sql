-- name: CreateLiveSession :one
INSERT INTO live_sessions (store_id, title, platform, platform_live_id, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetLiveSessionByID :one
SELECT * FROM live_sessions WHERE id = $1 AND store_id = $2;

-- name: GetActiveLiveSession :one
SELECT * FROM live_sessions
WHERE store_id = $1 AND platform_live_id = $2 AND status = 'active';

-- name: StartLiveSession :one
UPDATE live_sessions
SET status = 'live', started_at = now(), updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: EndLiveSession :one
UPDATE live_sessions
SET status = 'ended', ended_at = now(), updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: UpdateLiveSession :one
UPDATE live_sessions
SET title = $3, platform = $4, platform_live_id = $5, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: ListLiveSessionsByStore :many
SELECT * FROM live_sessions WHERE store_id = $1 ORDER BY started_at DESC;

-- name: UpsertDetectedOrder :one
-- Used when product_id is known (for upsert behavior)
INSERT INTO detected_orders (session_id, platform_user_id, platform_handle, comment_text, product_id, quantity)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, platform_user_id, product_id)
DO UPDATE SET quantity = detected_orders.quantity + EXCLUDED.quantity, comment_text = EXCLUDED.comment_text
RETURNING *;

-- name: InsertDetectedOrderWithoutProduct :one
-- Used for Instagram live comments where product_id is unknown
-- Updates quantity if an order already exists for this user in this session
INSERT INTO detected_orders (session_id, platform_user_id, platform_handle, comment_text, quantity)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (session_id, platform_user_id) WHERE product_id IS NULL
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
SELECT d.*, p.name AS product_name, p.price AS product_price
FROM detected_orders d
JOIN products p ON p.id = d.product_id
WHERE d.session_id = $1 AND d.platform_user_id = $2 AND d.cancelled = false;

-- name: UpdateDetectedOrderCartID :exec
UPDATE detected_orders SET cart_id = $2
WHERE session_id = $1 AND platform_user_id = $3 AND cancelled = false;

-- name: GetLiveSessionByPlatformLiveID :one
-- Busca live session pelo platform_live_id (usado por webhooks Instagram que não têm contexto de store)
SELECT * FROM live_sessions
WHERE platform_live_id = $1 AND status IN ('active', 'live')
ORDER BY created_at DESC
LIMIT 1;

-- name: IncrementLiveSessionComments :exec
UPDATE live_sessions
SET total_comments = total_comments + 1, updated_at = now()
WHERE id = $1;

-- name: IncrementLiveSessionOrders :exec
UPDATE live_sessions
SET total_orders = total_orders + 1, updated_at = now()
WHERE id = $1;

-- =============================================================================
-- LIVE SESSION PLATFORMS (multiple platform IDs per session)
-- =============================================================================

-- name: AddPlatformToSession :one
INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByPlatformLiveID :one
-- Find active live session by any associated platform_live_id
SELECT ls.*
FROM live_sessions ls
JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE lsp.platform_live_id = $1 AND ls.status IN ('active', 'live')
ORDER BY ls.created_at DESC
LIMIT 1;

-- name: ListPlatformsBySession :many
SELECT * FROM live_session_platforms
WHERE session_id = $1
ORDER BY added_at;

-- name: RemovePlatformFromSession :exec
DELETE FROM live_session_platforms
WHERE session_id = $1 AND platform_live_id = $2;

-- name: CountPlatformsBySession :one
SELECT COUNT(*)::int FROM live_session_platforms WHERE session_id = $1;
