-- =============================================================================
-- CARTS (belong to events, with optional session tracking)
-- =============================================================================

-- name: CreateCart :one
INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, status, expires_at)
VALUES ($1, $2, $3, $4, $5, 'active', $6)
RETURNING *;

-- name: GetCartByID :one
SELECT * FROM carts WHERE id = $1;

-- name: GetCartByToken :one
SELECT * FROM carts WHERE token = $1;

-- name: GetCartByEventAndUser :one
SELECT * FROM carts WHERE event_id = $1 AND platform_user_id = $2;

-- name: GetCartTotals :one
-- Returns total items and value for a cart (for notifications)
SELECT
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    COALESCE(SUM(ci.quantity * ci.unit_price), 0)::bigint AS total_value
FROM cart_items ci
WHERE ci.cart_id = $1;

-- name: UpdateCartStatus :one
UPDATE carts SET status = $2 WHERE id = $1 RETURNING *;

-- name: UpdateCartPayment :one
UPDATE carts
SET payment_status = $2, external_order_id = $3, paid_at = $4, payment_method = $5
WHERE id = $1
RETURNING *;

-- name: UpdateCartNotifyStatus :one
UPDATE carts
SET notify_status = $2, notify_error = $3, notified_at = $4
WHERE id = $1
RETURNING *;

-- name: ListCartsByEvent :many
SELECT * FROM carts WHERE event_id = $1 ORDER BY created_at;

-- name: CreateCartItem :one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpsertCartItem :one
-- Adds quantity to existing cart item or creates new one
-- waitlisted_quantity is added to existing (not replaced)
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (cart_id, product_id)
DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity,
             waitlisted_quantity = cart_items.waitlisted_quantity + EXCLUDED.waitlisted_quantity
RETURNING *;

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- name: FinalizeCartsByEvent :exec
-- Updates all active carts in an event to checkout status (live ended)
UPDATE carts
SET status = 'checkout'
WHERE event_id = $1 AND status = 'active';

-- name: CountCartsByEvent :one
SELECT COUNT(*)::int as count FROM carts WHERE event_id = $1 AND status = 'active';

-- name: UpdateCartItem :one
UPDATE cart_items
SET quantity = $2
WHERE id = $1
RETURNING *;

-- name: DeleteCartItem :exec
DELETE FROM cart_items WHERE id = $1;

-- name: GetCartItem :one
SELECT * FROM cart_items WHERE id = $1;

-- =============================================================================
-- EVENT DETAILS - Stats and Cart Listing
-- =============================================================================

-- name: GetEventStats :one
-- Returns stats for an event: comments, carts, revenue, products sold, funnel metrics
SELECT
    -- Funnel metrics
    COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = $1), 0)::int AS total_comments,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1), 0)::int AS total_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.status IN ('active', 'checkout')), 0)::int AS open_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND (ct.status = 'checkout' OR ct.checkout_url IS NOT NULL)), 0)::int AS checkout_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Product metrics
    COALESCE((
        SELECT SUM(ci.quantity)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.event_id = $1 AND ct.status != 'expired'
    ), 0)::int AS total_products_sold,
    -- Revenue metrics
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.event_id = $1 AND ct.status IN ('active', 'checkout')
    ), 0)::bigint AS projected_revenue,
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.event_id = $1 AND ct.payment_status = 'paid'
    ), 0)::bigint AS confirmed_revenue;

-- name: ListCartsWithTotalByEvent :many
-- Returns carts for an event with total value and item count (available vs waitlisted)
SELECT
    c.id,
    c.event_id,
    c.session_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.created_at,
    c.expires_at,
    COALESCE(SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price), 0)::bigint AS total_value,
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    COALESCE(SUM(ci.quantity - ci.waitlisted_quantity), 0)::int AS available_items,
    COALESCE(SUM(ci.waitlisted_quantity), 0)::int AS waitlisted_items
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC;

-- name: ListProductsByEvent :many
-- Returns products sold in an event with quantity and revenue
SELECT
    p.id,
    p.name,
    p.image_url,
    p.keyword,
    COALESCE(SUM(ci.quantity), 0)::int AS total_quantity,
    COALESCE(SUM(ci.quantity * ci.unit_price), 0)::bigint AS total_revenue
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id
JOIN products p ON p.id = ci.product_id
WHERE c.event_id = $1 AND c.status != 'expired'
GROUP BY p.id, p.name, p.image_url, p.keyword
ORDER BY total_quantity DESC;

-- =============================================================================
-- SESSION DETAILS - Stats per session
-- =============================================================================

-- name: GetSessionStats :one
-- Returns stats for a specific session: carts count and revenue
SELECT
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.session_id = $1), 0)::int AS total_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.session_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.session_id = $1 AND ct.status != 'expired'
    ), 0)::bigint AS total_revenue,
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.session_id = $1 AND ct.payment_status = 'paid'
    ), 0)::bigint AS paid_revenue;

-- =============================================================================
-- ERP SYNC & EXPIRATION
-- =============================================================================

-- name: UpdateCartExternalOrderID :exec
UPDATE carts SET external_order_id = $2 WHERE id = $1;

-- name: ListNonWaitlistedCartItems :many
-- Returns cart items that have available (non-waitlisted) quantity, with product external_id for ERP sync
-- Returns available_quantity = quantity - waitlisted_quantity
SELECT ci.id, ci.cart_id, ci.product_id,
       (ci.quantity - ci.waitlisted_quantity) AS quantity,
       ci.unit_price, ci.waitlisted_quantity,
       p.name AS product_name, p.external_id AS product_external_id,
       p.keyword AS product_keyword, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1 AND ci.quantity > ci.waitlisted_quantity;

-- name: ListExpiredCarts :many
-- Returns carts that have expired (active + past expires_at), with store_id from event
SELECT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
WHERE c.status = 'active' AND c.expires_at IS NOT NULL AND c.expires_at < now();

-- name: ListExpiredCartsByEventAndProduct :many
-- Returns expired carts for a specific event that contain a specific product (with available qty)
SELECT DISTINCT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
  AND c.status = 'active'
  AND c.expires_at IS NOT NULL
  AND c.expires_at < now()
  AND ci.product_id = $2
  AND ci.quantity > ci.waitlisted_quantity;

-- name: DeleteCartItemByCartAndProduct :exec
DELETE FROM cart_items WHERE cart_id = $1 AND product_id = $2;

-- name: UpdateCartItemWaitlistedQuantity :exec
UPDATE cart_items SET waitlisted_quantity = $3 WHERE cart_id = $1 AND product_id = $2;

-- name: GetCartByEventAndUserForUpdate :one
-- Lock the cart row for concurrent safety
SELECT * FROM carts WHERE event_id = $1 AND platform_user_id = $2 FOR UPDATE;

-- name: GetProductQuantityInUserCart :one
-- Returns the current quantity of a specific product in a user's cart for an event
SELECT COALESCE(ci.quantity, 0)::INT AS quantity
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id AND ci.product_id = $3
WHERE c.event_id = $1 AND c.platform_user_id = $2;

-- =============================================================================
-- PUBLIC CHECKOUT - Cart page for customers
-- =============================================================================

-- name: GetCartByTokenWithDetails :one
-- Returns cart with event info for public checkout page
SELECT
    c.id,
    c.event_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.checkout_url,
    c.checkout_id,
    c.checkout_expires_at,
    c.customer_email,
    c.payment_status,
    c.paid_at,
    c.created_at,
    c.expires_at,
    le.title AS event_title,
    le.store_id,
    s.name AS store_name,
    s.logo_url AS store_logo_url,
    s.cart_allow_edit AS allow_edit,
    s.cart_max_quantity_per_item AS max_quantity_per_item
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN stores s ON s.id = le.store_id
WHERE c.token = $1;

-- name: ListCartItemsForCheckout :many
-- Returns cart items with product details for checkout page
SELECT
    ci.id,
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price,
    ci.waitlisted_quantity,
    p.name AS product_name,
    p.image_url AS product_image_url,
    p.keyword AS product_keyword
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1
ORDER BY ci.id;

-- name: UpdateCartCustomerEmail :one
UPDATE carts
SET customer_email = $2
WHERE token = $1
RETURNING *;

-- name: UpdateCartCheckoutInfo :one
-- Updates checkout URL and ID after generating payment link
UPDATE carts
SET checkout_url = $2, checkout_id = $3, checkout_expires_at = $4
WHERE id = $1
RETURNING *;

-- name: GetCartByCheckoutID :one
-- Used by webhook to find cart when payment is confirmed
SELECT * FROM carts WHERE checkout_id = $1;

-- name: UpdateCartPaymentByCheckoutID :one
-- Updates payment status when webhook confirms payment
UPDATE carts
SET payment_status = $2, paid_at = $3
WHERE checkout_id = $1
RETURNING *;

-- name: UpdateCartPaymentStatus :one
-- Updates payment status directly by cart ID (for transparent checkout)
-- Uses checkout_id to store the payment ID from the provider
UPDATE carts
SET payment_status = $2, checkout_id = $3, paid_at = $4
WHERE id = $1
RETURNING *;

-- name: GetStorePaymentIntegration :one
-- Gets the active payment integration for a store
SELECT i.*
FROM integrations i
WHERE i.store_id = $1
  AND i.type = 'payment'
  AND i.status = 'active'
LIMIT 1;
