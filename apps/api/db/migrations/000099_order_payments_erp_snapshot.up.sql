-- Fatia 11b: order_payments passa a ser a fonte autoritativa dos dados de
-- finalização/NF do ERP pós-pagamento. Falta apenas o snapshot do gateway, que
-- hoje só existe no cart — as demais colunas (erp_finalisation_*, invoice_*) já
-- existem em order_payments desde a 000094 (até agora só espelhadas).
ALTER TABLE order_payments ADD COLUMN erp_payment_snapshot JSONB;

-- Backfill autoritativo a partir do cart de origem (orders.cart_id → carts).
-- Além do snapshot novo, re-sincroniza finalização/invoice para o caso de o
-- mirror ter projetado o estado no MOMENTO da materialização (pré-finalização)
-- e a finalização posterior ter escrito só no cart. Idempotente: quando cart e
-- order_payments já concordam, o UPDATE é um no-op de valor. NULL de snapshot é
-- ok (o cart só grava snapshot em falha de finalização).
UPDATE order_payments op
SET erp_payment_snapshot    = c.erp_payment_snapshot,
    erp_finalisation_status = c.erp_finalisation_status,
    erp_last_error          = c.erp_last_error,
    erp_last_attempt_at     = c.erp_last_attempt_at,
    erp_attempts_count      = c.erp_attempts_count,
    invoice_id              = c.erp_invoice_id,
    invoice_key             = c.erp_invoice_key,
    invoice_status          = c.erp_invoice_status,
    invoice_emitted_at      = c.erp_invoice_emitted_at
FROM orders o
JOIN carts c ON c.id = o.cart_id
WHERE op.order_id = o.id;
