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

-- =============================================================================
-- Fatia 11b — finalização/NF do ERP autoritativas em order_payments.
-- Todas keyed por cart_id (a finalização resolve o cart naturalmente) e
-- resolvem order_id via join orders→order_payments. NÃO tocam o cart: as
-- colunas de finalização/invoice deixam de ser escritas lá (a máquina de
-- reserva — erp_order_state/stock_launched/op_started_at/external_order_id —
-- continua no cart). No-op (0 rows) quando a Order ainda não foi materializada.
-- =============================================================================

-- name: MarkOrderERPFinalisationDone :exec
UPDATE order_payments op
SET erp_finalisation_status = 'done',
    erp_last_error          = NULL,
    erp_last_attempt_at     = now(),
    erp_attempts_count      = op.erp_attempts_count + 1
FROM orders o
WHERE o.id = op.order_id AND o.cart_id = $1;

-- name: MarkOrderERPFinalisationFailed :exec
-- erp_payment_snapshot é COALESCEd: o snapshot só é gravado na PRIMEIRA falha
-- (tentativa inicial), preservando a visão canônica do gateway para os retries.
UPDATE order_payments op
SET erp_finalisation_status = 'failed',
    erp_last_error          = $2,
    erp_last_attempt_at     = now(),
    erp_attempts_count      = op.erp_attempts_count + 1,
    erp_payment_snapshot    = COALESCE(op.erp_payment_snapshot, $3)
FROM orders o
WHERE o.id = op.order_id AND o.cart_id = $1;

-- name: MarkOrderERPFinalisationAttempt :exec
-- S1 da finalização retomável: persiste o snapshot ANTES de tocar o ERP e
-- carimba a tentativa. COALESCE preserva o snapshot da primeira tentativa;
-- erp_attempts_count NÃO é incrementado aqui (só registra o INÍCIO).
UPDATE order_payments op
SET erp_payment_snapshot = COALESCE(op.erp_payment_snapshot, $2),
    erp_last_attempt_at  = now()
FROM orders o
WHERE o.id = op.order_id AND o.cart_id = $1;

-- name: GetOrderERPFinalisationStatus :one
-- external_order_id continua autoritativo NO CART (coluna de reserva) — lido
-- aqui via join para o retry/idempotência decidirem resume vs. legado.
SELECT o.cart_id,
       op.erp_finalisation_status,
       op.erp_last_error,
       op.erp_last_attempt_at,
       op.erp_attempts_count,
       op.erp_payment_snapshot,
       COALESCE(c.external_order_id, '') AS external_order_id
FROM order_payments op
JOIN orders o ON o.id = op.order_id
JOIN carts  c ON c.id = o.cart_id
WHERE o.cart_id = $1;

-- name: UpsertOrderERPInvoice :execrows
-- NF autoritativa em order_payments. emitted_at é COALESCEd (só carimba a
-- primeira emissão autorizada). :execrows para o caller logar o skip benigno
-- quando não há Order para o external_order_id (NF é sempre pós-confirmação).
UPDATE order_payments op
SET invoice_id         = sqlc.narg('invoice_id'),
    invoice_key        = sqlc.narg('invoice_key'),
    invoice_status     = sqlc.narg('invoice_status'),
    invoice_emitted_at = COALESCE(op.invoice_emitted_at, sqlc.narg('emitted_at'))
FROM orders o
WHERE o.id = op.order_id AND o.cart_id = sqlc.arg('cart_id');

-- name: GetOrderERPInvoice :one
SELECT o.cart_id,
       op.invoice_id,
       op.invoice_key,
       op.invoice_status,
       op.invoice_emitted_at,
       COALESCE(c.external_order_id, '') AS external_order_id
FROM order_payments op
JOIN orders o ON o.id = op.order_id
JOIN carts  c ON c.id = o.cart_id
WHERE o.cart_id = $1;
