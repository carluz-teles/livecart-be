DROP INDEX IF EXISTS idx_carts_erp_order_state_inflight;
ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_erp_order_state_check;
ALTER TABLE carts
    DROP COLUMN IF EXISTS erp_op_started_at,
    DROP COLUMN IF EXISTS erp_stock_launched,
    DROP COLUMN IF EXISTS erp_order_state;
