-- Fatia 10-b (última do split Cart/Order): dropa as 9 colunas pós-venda do ERP
-- que ficaram MORTAS no cart. A Fatia 11b tornou order_payments a fonte
-- autoritativa da finalização/NF e repontou os wrappers Go (repository.go) para
-- as queries Order* (order_write.sql). O cart é a "order em draft" (pré-pagamento):
-- só o pós-venda sai. Ficam no cart as colunas de RESERVA (design C:
-- erp_order_state/erp_stock_launched/erp_op_started_at) e external_order_id.
--
-- As CHECK constraints inline (erp_finalisation_status IN (...), erp_invoice_status
-- IN (...)) caem junto com as respectivas colunas.

-- Índices parciais que dependem das colunas dropadas (069/072/084). Dropar a
-- coluna já os removeria em cascata; explicitamos para documentar o que sai.
DROP INDEX IF EXISTS idx_carts_erp_finalisation_failed;
DROP INDEX IF EXISTS idx_carts_awaiting_invoice;
DROP INDEX IF EXISTS idx_carts_paid_erp_in_flight;

ALTER TABLE carts
    DROP COLUMN IF EXISTS erp_finalisation_status,
    DROP COLUMN IF EXISTS erp_last_error,
    DROP COLUMN IF EXISTS erp_last_attempt_at,
    DROP COLUMN IF EXISTS erp_attempts_count,
    DROP COLUMN IF EXISTS erp_payment_snapshot,
    DROP COLUMN IF EXISTS erp_invoice_id,
    DROP COLUMN IF EXISTS erp_invoice_key,
    DROP COLUMN IF EXISTS erp_invoice_status,
    DROP COLUMN IF EXISTS erp_invoice_emitted_at;
