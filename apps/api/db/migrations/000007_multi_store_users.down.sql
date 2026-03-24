-- Revert multi-store changes

-- Drop invitations table
DROP TABLE IF EXISTS store_invitations;

-- Remove new columns from store_users
ALTER TABLE store_users DROP COLUMN IF EXISTS invited_at;
ALTER TABLE store_users DROP COLUMN IF EXISTS invited_by;
ALTER TABLE store_users DROP COLUMN IF EXISTS status;

-- Remove composite unique constraint
ALTER TABLE store_users DROP CONSTRAINT IF EXISTS store_users_store_clerk_unique;

-- Restore global unique constraint on clerk_user_id
ALTER TABLE store_users ADD CONSTRAINT store_users_clerk_user_id_key UNIQUE (clerk_user_id);
