-- name: GetDashboardStats :one
SELECT
    -- Total revenue from orders
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)
        FROM cart_items ci
        JOIN carts c ON c.id = ci.cart_id
        JOIN live_sessions ls ON ls.id = c.session_id
        WHERE ls.store_id = $1
    ), 0)::BIGINT as total_revenue,
    -- Total orders
    COALESCE((
        SELECT COUNT(*)
        FROM carts c
        JOIN live_sessions ls ON ls.id = c.session_id
        WHERE ls.store_id = $1
    ), 0)::INT as total_orders,
    -- Active products
    COALESCE((
        SELECT COUNT(*)
        FROM products p
        WHERE p.store_id = $1 AND p.active = true
    ), 0)::INT as active_products,
    -- Total lives
    COALESCE((
        SELECT COUNT(*)
        FROM live_sessions ls
        WHERE ls.store_id = $1
    ), 0)::INT as total_lives;

-- name: GetMonthlyRevenue :many
SELECT
    TO_CHAR(c.created_at, 'Mon') as month,
    EXTRACT(MONTH FROM c.created_at)::INT as month_num,
    COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as revenue
FROM carts c
JOIN live_sessions ls ON ls.id = c.session_id
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE ls.store_id = $1
  AND c.created_at >= date_trunc('year', CURRENT_DATE)
GROUP BY TO_CHAR(c.created_at, 'Mon'), EXTRACT(MONTH FROM c.created_at)
ORDER BY month_num;

-- name: GetTopProducts :many
SELECT
    p.id,
    p.name,
    p.keyword,
    COALESCE(SUM(ci.quantity), 0)::INT as total_sold,
    COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as total_revenue
FROM products p
JOIN cart_items ci ON ci.product_id = p.id
JOIN carts c ON c.id = ci.cart_id
JOIN live_sessions ls ON ls.id = c.session_id
WHERE ls.store_id = $1
GROUP BY p.id, p.name, p.keyword
ORDER BY total_sold DESC
LIMIT 5;
