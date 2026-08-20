DROP INDEX IF EXISTS carts_one_eternal_per_store_buyer;
ALTER TABLE carts DROP COLUMN IF EXISTS never_expires;
ALTER TABLE carts DROP COLUMN IF EXISTS store_id;
