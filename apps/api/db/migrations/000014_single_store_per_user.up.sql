-- Migration: Simplify to 1 user = 1 store
-- Each user can only have ONE membership (either as owner or member)

-- Step 1: Keep only the most recent membership per user
-- If user has multiple memberships, keep the one where they're owner (if any)
-- Otherwise keep the most recently updated one
WITH ranked_memberships AS (
    SELECT id, user_id,
           ROW_NUMBER() OVER (
               PARTITION BY user_id
               ORDER BY
                   CASE WHEN role = 'owner' THEN 0 ELSE 1 END,
                   updated_at DESC NULLS LAST,
                   created_at DESC
           ) as rn
    FROM memberships
)
DELETE FROM memberships
WHERE id IN (
    SELECT id FROM ranked_memberships WHERE rn > 1
);

-- Step 2: Drop the old composite unique constraint
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_user_store_unique;

-- Step 3: Add new UNIQUE constraint on user_id only (1 user = 1 membership)
ALTER TABLE memberships ADD CONSTRAINT memberships_user_id_unique UNIQUE (user_id);

-- Step 4: Remove last_accessed_at column (not needed with single store)
ALTER TABLE memberships DROP COLUMN IF EXISTS last_accessed_at;

-- Step 5: Keep the store_id unique per store constraint (for data integrity)
-- This ensures a user can't accidentally have same store twice
ALTER TABLE memberships ADD CONSTRAINT memberships_store_user_unique UNIQUE (store_id, user_id);
