-- Queries de escrita para materialização da Order (Fatia A).
-- Leitura da Order (read-models) permanece em order.sql até Fase B.

-- name: GetOrderIDByCartID :one
-- Idempotência: verifica se já existe Order para este cart.
SELECT id FROM orders WHERE cart_id = $1;

-- name: SetOrderLogisticsTrackingToken :exec
-- Fatia C1: grava o tracking_token na Order (order_logistics) — fonte da verdade
-- do rastreamento. Set-once: só escreve quando ainda nulo, e o UNIQUE INDEX
-- idx_order_logistics_tracking garante unicidade global. Keyed por cart_id
-- (join orders→order_logistics) para o postcheckout não resolver order_id fora.
-- No-op (0 rows) quando a Order ainda não foi materializada (path síncrono do
-- cartão) — a materialização posterior copia carts.tracking_token adiante.
UPDATE order_logistics ol
SET tracking_token = $2
FROM orders o
WHERE o.id = ol.order_id
  AND o.cart_id = $1
  AND ol.tracking_token IS NULL;

-- name: GetCartByOrderLogisticsTrackingToken :one
-- Fatia C1: acha o cart de origem a partir do tracking_token gravado na Order
-- (order_logistics). É o lookup canônico do rastreamento; o lookup por
-- carts.tracking_token vira fallback enquanto durar o dual-write (Fase F remove
-- o do cart).
SELECT c.* FROM carts c
JOIN orders o ON o.cart_id = c.id
JOIN order_logistics ol ON ol.order_id = o.id
WHERE ol.tracking_token = $1;

-- name: GetOrderStatusByCartID :one
-- Guarda de transição (Fatia E1): id + status atuais da Order do cart. O listener
-- usa o status para decidir idempotência (já terminal) vs transição inválida.
SELECT id, status FROM orders WHERE cart_id = $1;

-- name: SetOrderStatusByCartID :exec
-- Transição terminal da raiz Order (refunded/cancelled). As guardas
-- (idempotência, só-a-partir-de-paid) ficam no listener; aqui só o UPDATE.
UPDATE orders SET status = $2, updated_at = now() WHERE cart_id = $1;

-- name: SetOrderPaymentStatusByCartID :exec
-- Espelha o status terminal no order_payments.payment_status, keyed por cart_id.
UPDATE order_payments op
SET payment_status = $2
FROM orders o
WHERE o.id = op.order_id AND o.cart_id = $1;

-- name: InsertOrder :one
INSERT INTO orders (
    cart_id, short_id, store_id, event_id, customer_id, status,
    total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at,
    customer_snapshot
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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
    order_id, shipping_address, shipping_provider, shipping_service_id,
    shipping_service_name, shipping_carrier, shipping_cost_cents,
    shipping_cost_real_cents, shipping_deadline_days, tracking_token
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

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
