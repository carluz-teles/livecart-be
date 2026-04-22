-- =============================================================================
-- CUSTOMERS
-- =============================================================================

-- name: CreateCustomer :one
INSERT INTO customers (
    store_id,
    platform_user_id,
    platform_handle,
    email,
    phone,
    first_order_at,
    last_order_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpsertCustomer :one
-- Creates a new customer or updates existing one (by store_id + platform_user_id)
INSERT INTO customers (
    store_id,
    platform_user_id,
    platform_handle,
    email,
    phone,
    first_order_at,
    last_order_at
) VALUES ($1, $2, $3, $4, $5, now(), now())
ON CONFLICT (store_id, platform_user_id) DO UPDATE SET
    platform_handle = COALESCE(EXCLUDED.platform_handle, customers.platform_handle),
    email = COALESCE(EXCLUDED.email, customers.email),
    phone = COALESCE(EXCLUDED.phone, customers.phone),
    last_order_at = now(),
    updated_at = now()
RETURNING *;

-- name: GetCustomerByID :one
SELECT * FROM customers WHERE id = $1;

-- name: GetCustomerByPlatformUser :one
SELECT * FROM customers
WHERE store_id = $1 AND platform_user_id = $2;

-- name: GetCustomerByHandle :one
SELECT * FROM customers
WHERE store_id = $1 AND platform_handle = $2
LIMIT 1;

-- name: UpdateCustomer :exec
UPDATE customers SET
    platform_handle = COALESCE($2, platform_handle),
    email = COALESCE($3, email),
    phone = COALESCE($4, phone),
    updated_at = now()
WHERE id = $1;

-- name: UpdateCustomerLastOrder :exec
UPDATE customers SET
    last_order_at = now(),
    updated_at = now()
WHERE id = $1;

-- name: ListCustomers :many
-- List customers with aggregated order stats
SELECT
    c.*,
    COALESCE(stats.total_orders, 0)::INT as total_orders,
    COALESCE(stats.total_spent, 0)::BIGINT as total_spent
FROM customers c
LEFT JOIN LATERAL (
    SELECT
        COUNT(DISTINCT cart.id)::INT as total_orders,
        SUM(ci.quantity * ci.unit_price)::BIGINT as total_spent
    FROM carts cart
    JOIN cart_items ci ON ci.cart_id = cart.id
    WHERE cart.customer_id = c.id
) stats ON true
WHERE c.store_id = $1
ORDER BY c.last_order_at DESC NULLS LAST
LIMIT $2 OFFSET $3;

-- name: CountCustomers :one
SELECT COUNT(*)::int FROM customers WHERE store_id = $1;

-- name: GetCustomerStats :one
SELECT
    COUNT(*)::INT as total_customers,
    COUNT(CASE WHEN last_order_at > now() - interval '30 days' THEN 1 END)::INT as active_customers,
    COALESCE(
        (
            SELECT SUM(ci.quantity * ci.unit_price) / NULLIF(COUNT(DISTINCT c.id), 0)
            FROM carts cart
            JOIN cart_items ci ON ci.cart_id = cart.id
            JOIN customers c ON c.id = cart.customer_id
            WHERE c.store_id = $1
        ),
        0
    )::BIGINT as avg_spent_per_customer
FROM customers
WHERE store_id = $1;

-- name: SearchCustomers :many
SELECT
    c.*,
    COALESCE(stats.total_orders, 0)::INT as total_orders,
    COALESCE(stats.total_spent, 0)::BIGINT as total_spent
FROM customers c
LEFT JOIN LATERAL (
    SELECT
        COUNT(DISTINCT cart.id)::INT as total_orders,
        SUM(ci.quantity * ci.unit_price)::BIGINT as total_spent
    FROM carts cart
    JOIN cart_items ci ON ci.cart_id = cart.id
    WHERE cart.customer_id = c.id
) stats ON true
WHERE c.store_id = $1
  AND (c.platform_handle ILIKE $2 OR c.email ILIKE $2)
ORDER BY c.last_order_at DESC NULLS LAST
LIMIT $3 OFFSET $4;

-- name: DeleteCustomer :exec
DELETE FROM customers WHERE id = $1;
