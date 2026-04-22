-- Add customer and shipping address fields to carts.
-- These are captured by the frontend checkout form and used when creating the
-- paid sales order in the ERP after payment confirmation.

ALTER TABLE carts ADD COLUMN IF NOT EXISTS customer_name VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS customer_document VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS customer_phone VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS shipping_address JSONB;
