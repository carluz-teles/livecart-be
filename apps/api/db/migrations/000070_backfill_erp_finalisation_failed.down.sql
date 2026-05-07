-- Reverse the backfill by matching the canned error message we wrote in
-- the up migration. Rows that subsequent retries flipped to 'done' have
-- a different (or null) error message and are not touched.

UPDATE carts
SET
    erp_finalisation_status = 'pending',
    erp_last_error          = NULL,
    erp_last_attempt_at     = NULL,
    erp_attempts_count      = GREATEST(erp_attempts_count - 1, 0),
    erp_payment_snapshot    = NULL
WHERE erp_finalisation_status = 'failed'
  AND erp_last_error LIKE 'Pedido pago sem confirmação no ERP. Importado pelo backfill%';
