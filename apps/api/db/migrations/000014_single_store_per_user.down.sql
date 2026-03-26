-- Rollback: Restore multi-store support

-- Step 1: Drop the new single-user constraint
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_user_id_unique;

-- Step 2: Drop the store_user constraint
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_store_user_unique;

-- Step 3: Restore the original composite unique constraint
ALTER TABLE memberships ADD CONSTRAINT memberships_user_store_unique UNIQUE (user_id, store_id);

-- Step 4: Restore last_accessed_at column
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS last_accessed_at TIMESTAMPTZ;
