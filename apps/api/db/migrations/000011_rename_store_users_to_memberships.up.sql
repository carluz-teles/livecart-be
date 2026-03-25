-- Rename store_users to memberships for clarity
-- A membership represents a clerk_user's access to a store

-- Rename the table
ALTER TABLE store_users RENAME TO memberships;

-- Rename constraints
ALTER TABLE memberships RENAME CONSTRAINT store_users_pkey TO memberships_pkey;
ALTER TABLE memberships RENAME CONSTRAINT store_users_store_id_fkey TO memberships_store_id_fkey;
ALTER TABLE memberships RENAME CONSTRAINT store_users_store_clerk_unique TO memberships_store_clerk_unique;

-- Rename indexes (if any)
ALTER INDEX IF EXISTS idx_store_users_clerk_user_id RENAME TO idx_memberships_clerk_user_id;

-- Update store_invitations foreign key (invited_by references memberships)
ALTER TABLE store_invitations DROP CONSTRAINT IF EXISTS store_invitations_invited_by_fkey;
ALTER TABLE store_invitations ADD CONSTRAINT store_invitations_invited_by_fkey
    FOREIGN KEY (invited_by) REFERENCES memberships(id);

-- Remove onboarding_complete from stores (no longer needed)
ALTER TABLE stores DROP COLUMN IF EXISTS onboarding_complete;

-- Add last_accessed_at to memberships for tracking last store access
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS last_accessed_at TIMESTAMPTZ;
