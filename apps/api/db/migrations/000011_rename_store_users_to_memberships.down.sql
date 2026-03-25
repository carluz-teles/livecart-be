-- Revert: rename memberships back to store_users

-- Remove last_accessed_at
ALTER TABLE memberships DROP COLUMN IF EXISTS last_accessed_at;

-- Add onboarding_complete back to stores
ALTER TABLE stores ADD COLUMN IF NOT EXISTS onboarding_complete BOOLEAN NOT NULL DEFAULT false;

-- Revert store_invitations foreign key
ALTER TABLE store_invitations DROP CONSTRAINT IF EXISTS store_invitations_invited_by_fkey;
ALTER TABLE store_invitations ADD CONSTRAINT store_invitations_invited_by_fkey
    FOREIGN KEY (invited_by) REFERENCES store_users(id);

-- Rename indexes back
ALTER INDEX IF EXISTS idx_memberships_clerk_user_id RENAME TO idx_store_users_clerk_user_id;

-- Rename constraints back
ALTER TABLE memberships RENAME CONSTRAINT memberships_pkey TO store_users_pkey;
ALTER TABLE memberships RENAME CONSTRAINT memberships_store_id_fkey TO store_users_store_id_fkey;
ALTER TABLE memberships RENAME CONSTRAINT memberships_store_clerk_unique TO store_users_store_clerk_unique;

-- Rename table back
ALTER TABLE memberships RENAME TO store_users;
