-- =============================================================================
-- LIVE EVENTS
-- Container for live sessions. Carts are tied to events, not sessions.
-- =============================================================================

-- name: CreateLiveEvent :one
INSERT INTO live_events (store_id, title, status)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetLiveEventByID :one
SELECT * FROM live_events WHERE id = $1;

-- name: GetLiveEventByIDAndStore :one
SELECT * FROM live_events WHERE id = $1 AND store_id = $2;

-- name: GetActiveLiveEventByStore :one
SELECT * FROM live_events
WHERE store_id = $1 AND status = 'active'
ORDER BY created_at DESC
LIMIT 1;

-- name: EndLiveEvent :one
UPDATE live_events
SET status = 'ended', updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: UpdateLiveEventTitle :one
UPDATE live_events
SET title = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListLiveEventsByStore :many
SELECT * FROM live_events
WHERE store_id = $1
ORDER BY created_at DESC;

-- name: IncrementLiveEventOrders :exec
UPDATE live_events
SET total_orders = total_orders + 1, updated_at = now()
WHERE id = $1;

-- name: CountSessionsByEvent :one
SELECT COUNT(*)::int FROM live_sessions WHERE event_id = $1;

-- name: GetEventBySessionID :one
SELECT e.* FROM live_events e
JOIN live_sessions s ON s.event_id = e.id
WHERE s.id = $1;

-- name: GetEventByPlatformLiveID :one
-- Find active event by any associated platform_live_id (via session)
SELECT e.*
FROM live_events e
JOIN live_sessions s ON s.event_id = e.id
JOIN live_session_platforms lsp ON lsp.session_id = s.id
WHERE lsp.platform_live_id = $1 AND e.status = 'active'
ORDER BY e.created_at DESC
LIMIT 1;
