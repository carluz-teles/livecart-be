-- Add clerk_org_id to stores table
-- This links our local store to the Clerk Organization
ALTER TABLE stores
  ADD COLUMN IF NOT EXISTS clerk_org_id TEXT UNIQUE;

-- Index for looking up store by clerk org id
CREATE INDEX IF NOT EXISTS idx_stores_clerk_org_id ON stores(clerk_org_id);

-- Note: store_users and store_invitations tables will be deprecated
-- but not dropped yet. They'll be removed after validation.
-- DROP TABLE IF EXISTS store_invitations;
-- DROP TABLE IF EXISTS store_users;
