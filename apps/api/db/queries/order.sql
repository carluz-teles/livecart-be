-- name: ListOrdersByStore :many
SELECT c.*, ls.platform, ls.platform_live_id
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
WHERE ls.store_id = $1
ORDER BY c.created_at DESC;

-- name: GetOrderDetail :one
SELECT c.*, ls.platform, ls.platform_live_id, ls.store_id
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
WHERE c.id = $1;
