-- =============================================================================
-- WAITLIST ITEMS
-- =============================================================================

-- name: CreateWaitlistItem :one
INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetNextWaitlistPosition :one
SELECT COALESCE(MAX(position), 0) + 1 AS next_position
FROM waitlist_items
WHERE event_id = $1 AND product_id = $2;

-- name: GetFirstWaitingByProduct :one
SELECT * FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting'
ORDER BY position ASC
LIMIT 1;

-- name: UpdateWaitlistItemStatus :exec
UPDATE waitlist_items
SET status = $2,
    notified_at = $3,
    fulfilled_at = $4,
    expires_at = $5
WHERE id = $1;

-- name: ListWaitlistByEventAndUser :many
SELECT wi.*, p.name AS product_name, p.keyword AS product_keyword, p.image_url AS product_image_url
FROM waitlist_items wi
JOIN products p ON p.id = wi.product_id
WHERE wi.event_id = $1 AND wi.platform_user_id = $2
ORDER BY wi.created_at;

-- name: ListWaitlistByEventAndProduct :many
SELECT * FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting'
ORDER BY position ASC;

-- name: GetWaitlistItemByEventUserProduct :one
SELECT * FROM waitlist_items
WHERE event_id = $1 AND platform_user_id = $2 AND product_id = $3
  AND status IN ('waiting', 'notified');

-- name: ExpireWaitlistByEvent :exec
UPDATE waitlist_items
SET status = 'expired'
WHERE event_id = $1 AND status IN ('waiting', 'notified');

-- name: ListExpiredNotifiedWaitlistItems :many
SELECT * FROM waitlist_items
WHERE status = 'notified' AND expires_at IS NOT NULL AND expires_at < now();

-- name: CountWaitingByProduct :one
SELECT COUNT(*)::int FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting';
