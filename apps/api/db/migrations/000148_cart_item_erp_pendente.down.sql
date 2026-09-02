DROP INDEX IF EXISTS idx_cart_items_erp_pendente;
ALTER TABLE cart_items DROP COLUMN IF EXISTS erp_pending_since;
