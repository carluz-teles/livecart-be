-- =============================================================================
-- EVENT UPSELLS
-- =============================================================================

-- name: CreateEventUpsell :one
INSERT INTO event_upsells (event_id, product_id, discount_percent, message_template, display_order, active)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetEventUpsellByID :one
SELECT
    eu.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM event_upsells eu
JOIN products p ON p.id = eu.product_id
WHERE eu.id = $1;

-- name: ListEventUpsells :many
SELECT
    eu.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM event_upsells eu
JOIN products p ON p.id = eu.product_id
WHERE eu.event_id = $1
ORDER BY eu.display_order ASC, eu.created_at ASC;

-- name: ListActiveEventUpsells :many
-- Get active upsells with available stock
SELECT
    eu.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    (p.price * (100 - eu.discount_percent) / 100)::bigint AS discounted_price
FROM event_upsells eu
JOIN products p ON p.id = eu.product_id
WHERE eu.event_id = $1
    AND eu.active = true
    AND p.active = true
    AND p.stock > 0
ORDER BY eu.display_order ASC;

-- name: UpdateEventUpsell :one
UPDATE event_upsells
SET
    discount_percent = $3,
    message_template = $4,
    display_order = $5,
    active = $6,
    updated_at = now()
WHERE id = $1 AND event_id = $2
RETURNING *;

-- name: DeleteEventUpsell :exec
DELETE FROM event_upsells WHERE id = $1 AND event_id = $2;

-- name: DeleteEventUpsellByProductID :exec
DELETE FROM event_upsells WHERE event_id = $1 AND product_id = $2;

-- name: DeleteAllEventUpsells :exec
DELETE FROM event_upsells WHERE event_id = $1;

-- name: CountEventUpsells :one
SELECT COUNT(*)::int FROM event_upsells WHERE event_id = $1;

-- name: CountActiveEventUpsells :one
SELECT COUNT(*)::int
FROM event_upsells eu
JOIN products p ON p.id = eu.product_id
WHERE eu.event_id = $1 AND eu.active = true AND p.active = true AND p.stock > 0;

-- name: GetUpsellDiscountedPrice :one
-- Calculate discounted price for a specific upsell
SELECT
    eu.discount_percent,
    p.price AS original_price,
    (p.price * (100 - eu.discount_percent) / 100)::bigint AS discounted_price
FROM event_upsells eu
JOIN products p ON p.id = eu.product_id
WHERE eu.id = $1;

-- name: IsProductInEventUpsells :one
-- Check if product is already an upsell for this event
SELECT EXISTS (SELECT 1 FROM event_upsells WHERE event_id = $1 AND product_id = $2) AS is_upsell;
