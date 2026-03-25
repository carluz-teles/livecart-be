-- Migration: Create users table and normalize memberships
-- This migration removes dependency on Clerk Organizations
-- and creates a proper users table for our own user management

-- Step 1: Create users table
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    clerk_id TEXT UNIQUE NOT NULL,
    email TEXT UNIQUE NOT NULL,
    name TEXT,
    avatar_url TEXT,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

-- Indexes for users table
CREATE INDEX idx_users_clerk_id ON users(clerk_id);
CREATE INDEX idx_users_email ON users(email);

-- Step 2: Migrate existing users from memberships
-- Insert unique users based on clerk_user_id
-- Use clerk_user_id as email placeholder if email is empty
INSERT INTO users (clerk_id, email, name, avatar_url, created_at, updated_at)
SELECT DISTINCT ON (clerk_user_id)
    clerk_user_id,
    COALESCE(NULLIF(email, ''), clerk_user_id || '@placeholder.local'),
    name,
    avatar_url,
    created_at,
    COALESCE(updated_at, created_at)
FROM memberships
WHERE clerk_user_id IS NOT NULL
ORDER BY clerk_user_id, created_at ASC;

-- Step 3: Add user_id column to memberships
ALTER TABLE memberships ADD COLUMN user_id UUID REFERENCES users(id) ON DELETE CASCADE;

-- Step 4: Populate user_id from clerk_user_id
UPDATE memberships m
SET user_id = u.id
FROM users u
WHERE m.clerk_user_id = u.clerk_id;

-- Step 5: Make user_id NOT NULL (after data migration)
ALTER TABLE memberships ALTER COLUMN user_id SET NOT NULL;

-- Step 6: Drop redundant columns from memberships
ALTER TABLE memberships DROP COLUMN IF EXISTS clerk_user_id;
ALTER TABLE memberships DROP COLUMN IF EXISTS email;
ALTER TABLE memberships DROP COLUMN IF EXISTS name;
ALTER TABLE memberships DROP COLUMN IF EXISTS avatar_url;
ALTER TABLE memberships DROP COLUMN IF EXISTS password_hash;

-- Step 7: Update unique constraint
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_store_clerk_unique;
ALTER TABLE memberships ADD CONSTRAINT memberships_user_store_unique UNIQUE (user_id, store_id);

-- Step 8: Drop old index
DROP INDEX IF EXISTS idx_memberships_clerk_user_id;

-- Step 9: Create new index
CREATE INDEX idx_memberships_user_id ON memberships(user_id);

-- Step 10: Remove Clerk Organizations from stores
ALTER TABLE stores DROP COLUMN IF EXISTS clerk_org_id;
DROP INDEX IF EXISTS idx_stores_clerk_org_id;

-- Step 11: Update store_invitations foreign key constraint
-- The invited_by still references memberships(id) which is correct
