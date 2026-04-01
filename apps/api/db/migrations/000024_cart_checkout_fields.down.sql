-- Revert checkout-related fields from carts table
DROP INDEX IF EXISTS idx_carts_checkout_id;
ALTER TABLE carts DROP COLUMN IF EXISTS customer_email;
ALTER TABLE carts DROP COLUMN IF EXISTS checkout_expires_at;
ALTER TABLE carts DROP COLUMN IF EXISTS checkout_id;
