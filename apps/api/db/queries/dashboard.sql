-- name: GetDashboardStats :one
SELECT
    -- Grupo B correction: was summing ALL carts (unpaid + refunded); now counts only paid orders.
    COALESCE((
        SELECT SUM(o.total_cents)
        FROM orders o
        WHERE o.store_id = $1 AND o.status = 'paid'
    ), 0)::BIGINT as total_revenue,
    -- Total orders
    COALESCE((
        SELECT COUNT(*)
        FROM carts c
        JOIN live_events e ON e.id = c.event_id
        WHERE e.store_id = $1
    ), 0)::INT as total_orders,
    -- Active products
    COALESCE((
        SELECT COUNT(*)
        FROM products p
        WHERE p.store_id = $1 AND p.active = true
    ), 0)::INT as active_products,
    -- Total lives (events)
    COALESCE((
        SELECT COUNT(*)
        FROM live_events e
        WHERE e.store_id = $1
    ), 0)::INT as total_lives;

-- name: GetMonthlyRevenue :many
-- Grupo B correction: was summing ALL carts (no paid filter); now counts only paid orders.
SELECT
    TO_CHAR(o.paid_at, 'Mon') as month,
    EXTRACT(MONTH FROM o.paid_at)::INT as month_num,
    COALESCE(SUM(o.total_cents), 0)::BIGINT as revenue
FROM orders o
WHERE o.store_id = $1
  AND o.status = 'paid'
  AND o.paid_at >= date_trunc('year', CURRENT_DATE)
GROUP BY TO_CHAR(o.paid_at, 'Mon'), EXTRACT(MONTH FROM o.paid_at)
ORDER BY month_num;

-- name: GetTopProducts :many
-- TODO(gmv-canonical): total_revenue aqui é por-produto (GROUP BY product), não por-cart;
-- cart_product_total_cents não se aplica diretamente. Requer refactor separado.
SELECT
    p.id,
    p.name,
    p.keyword,
    COALESCE(SUM(ci.quantity), 0)::INT as total_sold,
    COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as total_revenue
FROM products p
JOIN cart_items ci ON ci.product_id = p.id
JOIN carts c ON c.id = ci.cart_id
JOIN live_events e ON e.id = c.event_id
WHERE e.store_id = $1
GROUP BY p.id, p.name, p.keyword
ORDER BY total_sold DESC
LIMIT 5;

-- =============================================================================
-- ANALYTICS - Revenue Attribution
-- =============================================================================

-- name: GetEventsWithRevenue :many
-- Returns all events with their revenue metrics for analytics
SELECT
    e.id,
    e.title,
    e.status,
    e.created_at,
    COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = e.id), 0)::int AS total_comments,
    COALESCE((SELECT COUNT(*) FROM carts c WHERE c.event_id = e.id), 0)::int AS total_carts,
    COALESCE((SELECT COUNT(*) FROM carts c WHERE c.event_id = e.id AND c.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Grupo A: reads from sealed orders (paid-only was already the intent)
    COALESCE((
        SELECT SUM(o.total_cents)
        FROM orders o
        WHERE o.event_id = e.id AND o.status = 'paid'
    ), 0)::bigint AS confirmed_revenue
FROM live_events e
WHERE e.store_id = $1
ORDER BY e.created_at DESC
LIMIT $2;

-- name: GetAggregatedFunnel :one
-- Returns aggregated funnel metrics for the store (last N days)
SELECT
    -- Total comments across all events
    COALESCE((
        SELECT SUM(ls.total_comments)
        FROM live_sessions ls
        JOIN live_events e ON e.id = ls.event_id
        WHERE e.store_id = $1 AND ls.created_at >= NOW() - INTERVAL '1 day' * $2
    ), 0)::int AS total_comments,
    -- Total carts created
    COALESCE((
        SELECT COUNT(*)
        FROM carts c
        JOIN live_events e ON e.id = c.event_id
        WHERE e.store_id = $1 AND c.created_at >= NOW() - INTERVAL '1 day' * $2
    ), 0)::int AS total_carts,
    -- Carts that reached checkout
    COALESCE((
        SELECT COUNT(*)
        FROM carts c
        JOIN live_events e ON e.id = c.event_id
        WHERE e.store_id = $1
          AND c.created_at >= NOW() - INTERVAL '1 day' * $2
          AND (c.status = 'checkout' OR c.checkout_url IS NOT NULL)
    ), 0)::int AS checkout_carts,
    -- Paid carts
    COALESCE((
        SELECT COUNT(*)
        FROM carts c
        JOIN live_events e ON e.id = c.event_id
        WHERE e.store_id = $1
          AND c.created_at >= NOW() - INTERVAL '1 day' * $2
          AND c.payment_status = 'paid'
    ), 0)::int AS paid_carts,
    -- Grupo A: confirmed revenue reads from sealed orders (paid-only was already the intent)
    COALESCE((
        SELECT SUM(o.total_cents)
        FROM orders o
        WHERE o.store_id = $1
          AND o.paid_at >= NOW() - INTERVAL '1 day' * $2
          AND o.status = 'paid'
    ), 0)::bigint AS confirmed_revenue,
    -- Grupo A: average ticket from sealed orders
    COALESCE((
        SELECT AVG(o.total_cents)
        FROM orders o
        WHERE o.store_id = $1
          AND o.paid_at >= NOW() - INTERVAL '1 day' * $2
          AND o.status = 'paid'
    ), 0)::bigint AS average_ticket;
