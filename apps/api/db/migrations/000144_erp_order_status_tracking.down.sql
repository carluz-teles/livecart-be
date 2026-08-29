DROP TABLE IF EXISTS erp_order_status_events;
DROP INDEX IF EXISTS idx_carts_erp_order_status_stale;
ALTER TABLE carts
    DROP COLUMN IF EXISTS erp_order_status,
    DROP COLUMN IF EXISTS erp_order_status_at,
    DROP COLUMN IF EXISTS erp_order_number;
