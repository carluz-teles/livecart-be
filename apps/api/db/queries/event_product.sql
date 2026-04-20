-- =============================================================================
-- EVENT PRODUCTS (Whitelist)
-- =============================================================================

-- name: CreateEventProduct :one
INSERT INTO event_products (event_id, product_id, special_price, max_quantity, display_order, featured)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetEventProductByID :one
SELECT
    ep.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM event_products ep
JOIN products p ON p.id = ep.product_id
WHERE ep.id = $1;

-- name: ListEventProducts :many
SELECT
    ep.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM event_products ep
JOIN products p ON p.id = ep.product_id
WHERE ep.event_id = $1
ORDER BY ep.display_order ASC, ep.created_at ASC;

-- name: ListFeaturedEventProducts :many
SELECT
    ep.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock
FROM event_products ep
JOIN products p ON p.id = ep.product_id
WHERE ep.event_id = $1 AND ep.featured = true AND p.active = true
ORDER BY ep.display_order ASC;

-- name: UpdateEventProduct :one
UPDATE event_products
SET
    special_price = $3,
    max_quantity = $4,
    display_order = $5,
    featured = $6,
    updated_at = now()
WHERE id = $1 AND event_id = $2
RETURNING *;

-- name: DeleteEventProduct :exec
DELETE FROM event_products WHERE id = $1 AND event_id = $2;

-- name: DeleteEventProductByProductID :exec
DELETE FROM event_products WHERE event_id = $1 AND product_id = $2;

-- name: DeleteAllEventProducts :exec
DELETE FROM event_products WHERE event_id = $1;

-- name: CountEventProducts :one
SELECT COUNT(*)::int FROM event_products WHERE event_id = $1;

-- name: GetEventProductByProductID :one
SELECT
    ep.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM event_products ep
JOIN products p ON p.id = ep.product_id
WHERE ep.event_id = $1 AND ep.product_id = $2;

-- name: HasEventProducts :one
-- Check if event has any products in whitelist
SELECT EXISTS (SELECT 1 FROM event_products WHERE event_id = $1) AS has_products;

-- name: IsProductInEventWhitelist :one
-- Check if product is in the event whitelist
SELECT EXISTS (SELECT 1 FROM event_products WHERE event_id = $1 AND product_id = $2) AS in_whitelist;

-- name: GetEffectiveProductPrice :one
-- Get the effective price for a product in an event (special_price or original)
SELECT COALESCE(ep.special_price, p.price)::bigint AS effective_price
FROM products p
LEFT JOIN event_products ep ON ep.product_id = p.id AND ep.event_id = $1
WHERE p.id = $2;

-- name: GetEffectiveMaxQuantity :one
-- Get the effective max quantity for a product in an event
-- Priority: event_product.max_quantity > live_event.cart_max_quantity_per_item > store.cart_max_quantity_per_item
SELECT COALESCE(
    ep.max_quantity,
    e.cart_max_quantity_per_item,
    s.cart_max_quantity_per_item,
    5  -- fallback default
)::int AS max_quantity
FROM live_events e
JOIN stores s ON s.id = e.store_id
LEFT JOIN event_products ep ON ep.event_id = e.id AND ep.product_id = $2
WHERE e.id = $1;

-- name: GetEventProductConfig :one
-- Get full product config for cart validation
SELECT
    p.id AS product_id,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.stock AS product_stock,
    p.active AS product_active,
    ep.special_price,
    ep.max_quantity,
    COALESCE(ep.special_price, p.price)::bigint AS effective_price,
    CASE
        WHEN NOT EXISTS (SELECT 1 FROM event_products ep2 WHERE ep2.event_id = $1) THEN true
        WHEN ep.id IS NOT NULL THEN true
        ELSE false
    END AS is_allowed
FROM products p
LEFT JOIN event_products ep ON ep.product_id = p.id AND ep.event_id = $1
WHERE p.id = $2 AND p.store_id = $3;
