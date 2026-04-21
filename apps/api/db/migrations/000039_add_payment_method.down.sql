-- Remove payment_method column from carts table
DROP INDEX IF EXISTS idx_carts_payment_method;
ALTER TABLE carts DROP COLUMN IF EXISTS payment_method;
