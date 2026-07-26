DROP INDEX IF EXISTS shipments_orders_order_id_idx;
ALTER TABLE shipments DROP COLUMN IF EXISTS orders_order_id;
