DROP INDEX IF EXISTS idx_carts_awaiting_invoice;

ALTER TABLE carts
    DROP COLUMN IF EXISTS erp_invoice_emitted_at,
    DROP COLUMN IF EXISTS erp_invoice_status,
    DROP COLUMN IF EXISTS erp_invoice_key,
    DROP COLUMN IF EXISTS erp_invoice_id;
