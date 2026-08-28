ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_join_not_self;
DROP INDEX IF EXISTS idx_carts_joined_to;
ALTER TABLE carts DROP COLUMN IF EXISTS joined_at, DROP COLUMN IF EXISTS joined_to_cart_id;
