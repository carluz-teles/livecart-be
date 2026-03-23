-- Remove columns from store_users
ALTER TABLE store_users DROP COLUMN IF EXISTS updated_at;
ALTER TABLE store_users DROP COLUMN IF EXISTS avatar_url;
ALTER TABLE store_users DROP COLUMN IF EXISTS name;
DROP INDEX IF EXISTS idx_store_users_clerk_user_id;
ALTER TABLE store_users DROP COLUMN IF EXISTS clerk_user_id;
