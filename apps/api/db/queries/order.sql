-- name: ListOrders :many
SELECT
    c.id,
    c.event_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.paid_at,
    c.created_at,
    c.expires_at,
    e.title as live_title,
    COALESCE(
        (SELECT lsp.platform FROM live_session_platforms lsp
         JOIN live_sessions ls ON ls.id = lsp.session_id
         WHERE ls.event_id = e.id
         ORDER BY lsp.added_at LIMIT 1),
        'instagram'
    ) as live_platform,
    COALESCE(
        (SELECT SUM(ci.quantity * ci.unit_price)::BIGINT FROM cart_items ci WHERE ci.cart_id = c.id),
        0
    ) as total_amount,
    COALESCE(
        (SELECT SUM(ci.quantity)::INT FROM cart_items ci WHERE ci.cart_id = c.id),
        0
    ) as total_items
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE e.store_id = $1
ORDER BY c.created_at DESC;

-- name: GetOrderByID :one
SELECT
    c.id,
    c.event_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.paid_at,
    c.created_at,
    c.expires_at,
    e.title as live_title,
    COALESCE(
        (SELECT lsp.platform FROM live_session_platforms lsp
         JOIN live_sessions ls ON ls.id = lsp.session_id
         WHERE ls.event_id = e.id
         ORDER BY lsp.added_at LIMIT 1),
        'instagram'
    ) as live_platform,
    e.store_id
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE c.id = $1;

-- name: GetOrderItems :many
SELECT
    ci.id,
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price,
    p.name as product_name,
    p.image_url as product_image,
    p.keyword as product_keyword
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- name: UpdateOrderStatus :one
UPDATE carts
SET status = $2
WHERE id = $1
RETURNING *;

-- name: UpdateOrderPaymentStatus :one
UPDATE carts
SET payment_status = $2, paid_at = CASE WHEN $2 = 'paid' THEN now() ELSE paid_at END
WHERE id = $1
RETURNING *;

-- name: GetOrderStats :one
SELECT
    COUNT(*)::INT as total_orders,
    COUNT(*) FILTER (WHERE c.status = 'pending')::INT as pending_orders,
    COALESCE(SUM(
        (SELECT SUM(ci.quantity * ci.unit_price) FROM cart_items ci WHERE ci.cart_id = c.id)
    ), 0)::BIGINT as total_revenue,
    COALESCE(
        CASE
            WHEN COUNT(*) > 0 THEN
                SUM((SELECT SUM(ci.quantity * ci.unit_price) FROM cart_items ci WHERE ci.cart_id = c.id)) / COUNT(*)
            ELSE 0
        END,
        0
    )::BIGINT as avg_ticket
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE e.store_id = $1;
