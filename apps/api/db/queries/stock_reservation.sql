-- name: CreateStockReservation :one
INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListActiveReservationsByEvent :many
SELECT * FROM stock_reservations WHERE event_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCart :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCartAndProduct :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND product_id = $2 AND status = 'active' ORDER BY created_at ASC;

-- name: ReverseReservationsByCart :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND status = 'active';

-- name: ReverseReservationsByCartAndProduct :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND product_id = $2 AND status = 'active';

-- name: ConvertReservationsByEvent :exec
UPDATE stock_reservations SET status = 'converted', reversed_at = now()
WHERE event_id = $1 AND status = 'active';

-- name: AdjustActiveReservationQuantity :one
-- Atomically increase or decrease the active reservation quantity for a
-- (cart, product) pair. delta_qty can be negative. Returns the row whose
-- quantity was adjusted (callers can flip status when it hits zero).
UPDATE stock_reservations
SET quantity = quantity + sqlc.arg(delta_qty)::int,
    erp_movement_id = COALESCE(NULLIF(sqlc.arg(erp_movement_id)::text, ''), erp_movement_id)
WHERE cart_id = sqlc.arg(cart_id) AND product_id = sqlc.arg(product_id) AND status = 'active'
RETURNING *;

-- name: HasActiveEventForProduct :one
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN live_events le ON le.id = sr.event_id
    WHERE sr.external_product_id = $1 AND sr.status = 'active' AND le.status = 'active'
) AS has_active;

-- name: HasStockGuardForProduct :one
-- Guard do sync de estoque vindo de webhook: TRUE quando o valor absoluto
-- reportado pelo ERP não pode sobrescrever o contador local — (a) live ativa
-- com reserva ativa segurando o produto (mesma semântica do antigo
-- HasActiveEventForProduct, agora com escopo por loja), ou (b) cart pago com
-- finalização ERP em voo ou falha recente contendo o produto: a reversão das
-- reservas na finalização infla o saldo do ERP por alguns segundos e o
-- overwrite local promoveria a waitlist contra unidades já vendidas.
SELECT (
    EXISTS(
        SELECT 1 FROM stock_reservations sr
        JOIN live_events le ON le.id = sr.event_id
        JOIN products p ON p.id = sr.product_id
        WHERE sr.external_product_id = sqlc.arg(external_product_id)
          AND p.store_id = sqlc.arg(store_id)
          AND sr.status = 'active'
          AND le.status = 'active')
    OR EXISTS(
        -- Fatia 11b: finalização autoritativa em order_payments (join via Order);
        -- COALESCE('pending') cobre o cart pago cuja Order ainda materializa. As
        -- colunas de reserva (erp_order_state/erp_op_started_at) seguem no cart.
        SELECT 1 FROM carts c
        JOIN cart_items ci ON ci.cart_id = c.id
        JOIN products p2 ON p2.id = ci.product_id
        LEFT JOIN orders o          ON o.cart_id  = c.id
        LEFT JOIN order_payments op ON op.order_id = o.id
        WHERE p2.store_id = sqlc.arg(store_id)
          AND p2.external_source = sqlc.arg(external_source)
          AND p2.external_id = sqlc.arg(external_product_id)
          AND ((c.payment_status = 'paid'
                AND COALESCE(op.erp_finalisation_status, 'pending') <> 'done'
                AND (c.paid_at > now() - interval '30 minutes'
                     OR op.erp_last_attempt_at > now() - interval '30 minutes'))
               -- design C: conversão/mutação em voo também movimenta o saldo
               -- transitoriamente (estornos do ciclo) — mesma supressão
               OR (c.erp_order_state IN ('converting','mutating')
                   AND c.erp_op_started_at > now() - interval '30 minutes')))
)::bool AS guarded;

-- name: ReverseReservationByID :exec
-- Marcação per-row da finalização retomável: cada reserva só vira 'reversed'
-- depois que o Tiny confirmou a entrada E correspondente — o resume estorna
-- apenas as que continuam 'active'.
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE id = $1 AND status = 'active';

-- name: HasPendingCartReversalForProduct :one
-- TRUE quando existe unidade JÁ CREDITADA no estoque local cujo estorno no ERP
-- ainda não terminou. Nessa janela o sync tem de ser SUPRIMIDO por inteiro —
-- não "só reduções".
--
-- Por que a distinção importa. Cancelar carrinho credita o local de uma vez,
-- dentro de uma transação; o estorno no Tiny sai um a um, por HTTP, fora dela.
-- Cada estorno dispara um webhook que nos faz reler o saldo ABSOLUTO do ERP —
-- que naquele instante está atrasado em relação a nós, e atrasado por nossa
-- causa. O `downgrade_only` lê "ERP menor que o local" como redução legítima do
-- lojista e grava o valor do ERP.
--
-- Foi assim que 1001 e 1004 caíram de 5 para 4 em staging (03/08): dois
-- estornos pendentes cada, o primeiro webhook chegou com o Tiny uma unidade
-- atrás e cravou o valor. E a mesma regra depois IMPEDE a autocorreção: quando
-- o Tiny alcança 5, `5 >= 4` preserva o 4 errado. O erro é gravado por uma
-- regra e protegido por ela.
--
-- Só cart em estado TERMINAL entra: em cart vivo a reserva ativa é normal e já
-- é coberta pelo primeiro EXISTS de HasStockGuardForProduct.
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN carts c ON c.id = sr.cart_id
    JOIN products p ON p.id = sr.product_id
    WHERE sr.external_product_id = sqlc.arg(external_product_id)
      AND p.store_id = sqlc.arg(store_id)
      AND sr.status = 'active'
      AND c.status IN ('cancelled', 'expired')
) AS has_pending;
