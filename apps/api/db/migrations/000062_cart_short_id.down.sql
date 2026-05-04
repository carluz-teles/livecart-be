DROP INDEX IF EXISTS carts_short_id_idx;
ALTER TABLE carts DROP COLUMN IF EXISTS short_id;
DROP TABLE IF EXISTS store_order_counters;
