-- name: CreateERPStockMovement :one
-- A intenção, gravada ANTES da chamada ao ERP. Ver a migration 000132.
INSERT INTO erp_stock_movements (
    store_id, cart_id, event_id, product_id, external_product_id,
    direction, quantity, unit_price_cents
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: MarkERPStockMovementConfirmed :exec
UPDATE erp_stock_movements
SET status = 'confirmed', erp_movement_id = $2, last_error = NULL, updated_at = now()
WHERE id = $1;

-- name: MarkERPStockMovementOutcome :exec
-- failed | unconfirmed, com o erro que levou até lá. attempts conta as
-- tentativas de EXECUÇÃO (a inline e as do resolver), para o teto de desistir.
UPDATE erp_stock_movements
SET status = $2, last_error = $3, attempts = attempts + 1,
    last_attempt_at = now(), updated_at = now()
WHERE id = $1;

-- name: GetERPStockMovement :one
SELECT * FROM erp_stock_movements WHERE id = $1;

-- name: ClaimERPStockMovement :one
-- Reivindica a linha (CAS: só se ela estiver no estado esperado) — mesmo
-- desenho do claim-first da reversão de reservas: dois resolvers (o agendado e
-- o inline do pagamento) podem mirar a mesma linha, e quem não reivindicou não
-- age. Os guards de idade impedem reivindicar trabalho que ainda está em voo:
-- 'pending' recente pertence à goroutine que fez a chamada; 'resolving' recente
-- pertence a outro resolver. Envelhecidos são de processos mortos.
UPDATE erp_stock_movements
SET status = 'resolving', last_attempt_at = now(), updated_at = now()
WHERE id = $1
  AND status = sqlc.arg(from_status)::varchar
  AND (sqlc.arg(from_status)::varchar <> 'pending' OR created_at < now() - interval '2 minutes')
  AND (sqlc.arg(from_status)::varchar <> 'resolving' OR last_attempt_at < now() - interval '5 minutes')
RETURNING *;

-- name: ListUnresolvedERPStockMovementsByCart :many
-- O gate da finalização: nada de pedido pago enquanto houver movimento em
-- dúvida para o carrinho. 'pending' recente conta — pode ser chamada em voo.
SELECT * FROM erp_stock_movements
WHERE cart_id = $1 AND status IN ('pending', 'failed', 'unconfirmed', 'resolving')
ORDER BY created_at ASC;

-- name: ListUnresolvedERPStockMovements :many
-- Visibilidade: o que está parado, para log/painel/alerta.
SELECT * FROM erp_stock_movements
WHERE status IN ('pending', 'failed', 'unconfirmed', 'resolving')
  AND created_at < now() - interval '1 minute'
ORDER BY created_at ASC
LIMIT $1;
