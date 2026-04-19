-- =============================================================================
-- NOTIFICATION LOGS
-- =============================================================================

-- name: CreateNotificationLog :one
INSERT INTO notification_logs (
    store_id, event_id, cart_id, platform_user_id, platform_handle,
    notification_type, channel, status, message_text
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateNotificationLogStatus :exec
UPDATE notification_logs
SET status = $2, sent_at = $3, error_message = $4
WHERE id = $1;

-- name: GetLastNotificationForUser :one
-- Returns the most recent notification for a user in a store (for cooldown check)
SELECT * FROM notification_logs
WHERE store_id = $1 AND platform_user_id = $2 AND status = 'sent'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetNotificationByCartAndType :one
-- Check if a notification of this type was already sent for this cart
SELECT * FROM notification_logs
WHERE cart_id = $1 AND notification_type = $2 AND status = 'sent'
LIMIT 1;

-- name: ListNotificationsByEvent :many
-- List all notifications for an event (for analytics)
SELECT * FROM notification_logs
WHERE event_id = $1
ORDER BY created_at DESC;

-- name: ListNotificationsByStore :many
-- List recent notifications for a store
SELECT * FROM notification_logs
WHERE store_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: CountNotificationsByStatus :one
-- Count notifications by status for a store
SELECT
    COUNT(*) FILTER (WHERE status = 'sent')::int AS sent,
    COUNT(*) FILTER (WHERE status = 'failed')::int AS failed,
    COUNT(*) FILTER (WHERE status = 'cooldown')::int AS cooldown_skipped
FROM notification_logs
WHERE store_id = $1 AND created_at > $2;

-- =============================================================================
-- STORE NOTIFICATION SETTINGS
-- =============================================================================

-- name: GetStoreNotificationSettings :one
SELECT notification_settings FROM stores WHERE id = $1;

-- name: UpdateStoreNotificationSettings :exec
UPDATE stores SET notification_settings = $2 WHERE id = $1;
