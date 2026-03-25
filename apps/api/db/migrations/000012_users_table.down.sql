-- Rollback: Remove users table and restore memberships structure
-- WARNING: This will lose data if users were created after migration

-- Step 1: Add back columns to memberships
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS clerk_user_id TEXT;
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS name TEXT;
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS avatar_url TEXT;
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS password_hash TEXT;

-- Step 2: Populate from users table
UPDATE memberships m
SET
    clerk_user_id = u.clerk_id,
    email = u.email,
    name = u.name,
    avatar_url = u.avatar_url
FROM users u
WHERE m.user_id = u.id;

-- Step 3: Drop user_id column
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_user_store_unique;
ALTER TABLE memberships DROP COLUMN IF EXISTS user_id;

-- Step 4: Restore old constraint
ALTER TABLE memberships ADD CONSTRAINT memberships_store_clerk_unique
    UNIQUE (store_id, clerk_user_id);

-- Step 5: Restore old index
CREATE INDEX IF NOT EXISTS idx_memberships_clerk_user_id ON memberships(clerk_user_id);

-- Step 6: Add back clerk_org_id to stores
ALTER TABLE stores ADD COLUMN IF NOT EXISTS clerk_org_id TEXT UNIQUE;
CREATE INDEX IF NOT EXISTS idx_stores_clerk_org_id ON stores(clerk_org_id);

-- Step 7: Drop new index
DROP INDEX IF EXISTS idx_memberships_user_id;

-- Step 8: Drop users table
DROP TABLE IF EXISTS users;
