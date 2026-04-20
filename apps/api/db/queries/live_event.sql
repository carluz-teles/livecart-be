-- =============================================================================
-- LIVE EVENTS
-- Container for live sessions. Carts are tied to events, not sessions.
-- =============================================================================

-- name: CreateLiveEvent :one
INSERT INTO live_events (
    store_id,
    title,
    type,
    status,
    close_cart_on_event_end,
    cart_expiration_minutes,
    cart_max_quantity_per_item,
    send_on_live_end
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
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

-- name: GetEventCartSettings :one
-- Get cart settings for an event with fallback to store defaults
SELECT
    e.id AS event_id,
    e.store_id,
    e.close_cart_on_event_end,
    COALESCE(e.cart_expiration_minutes, s.cart_expiration_minutes) AS cart_expiration_minutes,
    COALESCE(e.cart_max_quantity_per_item, s.cart_max_quantity_per_item) AS cart_max_quantity_per_item,
    COALESCE(e.send_on_live_end, s.send_on_live_end) AS send_on_live_end
FROM live_events e
JOIN stores s ON s.id = e.store_id
WHERE e.id = $1;

-- =============================================================================
-- LIVE MODE - Active Product and Processing Control
-- =============================================================================

-- name: SetActiveProduct :one
UPDATE live_events
SET current_active_product_id = $2, updated_at = now()
WHERE id = $1 AND store_id = $3
RETURNING *;

-- name: ClearActiveProduct :one
UPDATE live_events
SET current_active_product_id = NULL, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: SetProcessingPaused :one
UPDATE live_events
SET processing_paused = $2, updated_at = now()
WHERE id = $1 AND store_id = $3
RETURNING *;

-- name: GetLiveModeState :one
SELECT
    e.id,
    e.processing_paused,
    e.current_active_product_id,
    p.name AS active_product_name,
    p.keyword AS active_product_keyword,
    p.price AS active_product_price,
    p.image_url AS active_product_image_url
FROM live_events e
LEFT JOIN products p ON p.id = e.current_active_product_id
WHERE e.id = $1 AND e.store_id = $2;

-- =============================================================================
-- EVENT SCHEDULING & DESCRIPTION
-- =============================================================================

-- name: CreateLiveEventFull :one
INSERT INTO live_events (
    store_id,
    title,
    type,
    status,
    close_cart_on_event_end,
    cart_expiration_minutes,
    cart_max_quantity_per_item,
    send_on_live_end,
    scheduled_at,
    description
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: UpdateLiveEventDetails :one
UPDATE live_events
SET
    title = COALESCE($3, title),
    description = $4,
    scheduled_at = $5,
    updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: GetScheduledEvents :many
SELECT * FROM live_events
WHERE store_id = $1 AND scheduled_at IS NOT NULL AND status = 'scheduled'
ORDER BY scheduled_at ASC;

-- name: ListEventsReadyToStart :many
-- Find scheduled events that should be started (scheduled_at <= now)
SELECT * FROM live_events
WHERE status = 'scheduled' AND scheduled_at <= now()
ORDER BY scheduled_at ASC;

-- name: ActivateScheduledEvent :one
UPDATE live_events
SET status = 'active', updated_at = now()
WHERE id = $1 AND status = 'scheduled'
RETURNING *;

-- name: GetLiveEventWithCounts :one
SELECT
    e.*,
    (SELECT COUNT(*)::int FROM event_products WHERE event_id = e.id) AS product_count,
    (SELECT COUNT(*)::int FROM event_upsells WHERE event_id = e.id) AS upsell_count
FROM live_events e
WHERE e.id = $1 AND e.store_id = $2;
