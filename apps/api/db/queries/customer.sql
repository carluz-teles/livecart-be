-- name: ListCustomers :many
SELECT
    c.platform_user_id as id,
    c.platform_handle as handle,
    COUNT(DISTINCT c.id)::INT as total_orders,
    COALESCE(SUM(
        (SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
    ), 0)::BIGINT as total_spent,
    MAX(c.created_at) as last_order_at,
    MIN(c.created_at) as first_order_at
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
WHERE ls.store_id = $1
GROUP BY c.platform_user_id, c.platform_handle
ORDER BY last_order_at DESC;

-- name: GetCustomerByID :one
SELECT
    c.platform_user_id as id,
    c.platform_handle as handle,
    COUNT(DISTINCT c.id)::INT as total_orders,
    COALESCE(SUM(
        (SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
    ), 0)::BIGINT as total_spent,
    MAX(c.created_at) as last_order_at,
    MIN(c.created_at) as first_order_at
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
WHERE ls.store_id = $1 AND c.platform_user_id = $2
GROUP BY c.platform_user_id, c.platform_handle;

-- name: GetCustomerStats :one
SELECT
    COUNT(DISTINCT c.platform_user_id)::INT as total_customers,
    COUNT(DISTINCT CASE
        WHEN c.created_at > now() - interval '30 days' THEN c.platform_user_id
    END)::INT as active_customers,
    COALESCE(
        CASE
            WHEN COUNT(DISTINCT c.platform_user_id) > 0 THEN
                SUM((SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id))
                / COUNT(DISTINCT c.platform_user_id)
            ELSE 0
        END,
        0
    )::BIGINT as avg_spent_per_customer
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
WHERE ls.store_id = $1;
