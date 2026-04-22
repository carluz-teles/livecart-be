-- Remove customer_id from carts
DROP INDEX IF EXISTS idx_carts_customer_id;
ALTER TABLE carts DROP COLUMN IF EXISTS customer_id;

-- Drop customers table
DROP TABLE IF EXISTS customers;
