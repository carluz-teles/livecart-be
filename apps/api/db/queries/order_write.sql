-- Queries de escrita para materialização da Order (Fase A — OnCartPaid reactor).
-- Leitura da Order (read-models) permanece em order.sql até Fase B.

-- name: GetOrderIDByCartID :one
-- Idempotência: verifica se já existe Order para este cart.
SELECT id FROM orders WHERE cart_id = $1;

-- name: InsertOrder :one
INSERT INTO orders (
    cart_id, short_id, store_id, event_id, customer_id, status,
    total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: InsertOrderItem :exec
INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
VALUES ($1, $2, $3, $4, $5);

-- name: InsertOrderPayment :exec
INSERT INTO order_payments (
    order_id, payment_status, payment_method, card_snapshot, gateway_snapshot,
    coupon_id, coupon_code, coupon_discount_cents
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertOrderLogistics :exec
INSERT INTO order_logistics (
    order_id, shipping_address, shipping_service_id, shipping_service_name,
    shipping_carrier, shipping_cost_cents, shipping_cost_real_cents,
    shipping_deadline_days, tracking_token
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetCartItemsForOrderMaterialization :many
-- Retorna os itens do cart com o nome do produto denormalizado (snapshot).
SELECT
    ci.product_id,
    p.name  AS product_name,
    ci.quantity::int    AS quantity,
    COALESCE(ci.unit_price, 0)::bigint AS unit_price
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- name: GetPaidCartsWithoutOrder :many
-- Backfill: carts pagas que ainda não têm Order materializada.
SELECT c.id
FROM carts c
LEFT JOIN orders o ON o.cart_id = c.id
WHERE c.payment_status = 'paid'
  AND o.id IS NULL;
