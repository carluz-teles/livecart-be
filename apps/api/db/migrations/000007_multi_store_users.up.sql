-- Enable multi-store: a user can belong to multiple stores
-- Remove global UNIQUE constraint on clerk_user_id
-- Add composite UNIQUE on (store_id, clerk_user_id)

-- Drop the existing unique constraint on clerk_user_id
ALTER TABLE store_users DROP CONSTRAINT IF EXISTS store_users_clerk_user_id_key;

-- Add composite unique constraint (user can only be in a store once)
ALTER TABLE store_users ADD CONSTRAINT store_users_store_clerk_unique
    UNIQUE (store_id, clerk_user_id);

-- Add status column for invitations (active, pending, inactive)
ALTER TABLE store_users ADD COLUMN IF NOT EXISTS status VARCHAR NOT NULL DEFAULT 'active';

-- Add invited_by column to track who invited the user
ALTER TABLE store_users ADD COLUMN IF NOT EXISTS invited_by UUID REFERENCES store_users(id);

-- Add invited_at timestamp
ALTER TABLE store_users ADD COLUMN IF NOT EXISTS invited_at TIMESTAMPTZ;

-- Create invitations table for pending invites
CREATE TABLE IF NOT EXISTS store_invitations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id      UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    email         VARCHAR NOT NULL,
    role          VARCHAR NOT NULL DEFAULT 'member',
    token         VARCHAR(64) UNIQUE NOT NULL,
    invited_by    UUID NOT NULL REFERENCES store_users(id),
    status        VARCHAR NOT NULL DEFAULT 'pending', -- pending, accepted, expired, revoked
    expires_at    TIMESTAMPTZ NOT NULL,
    accepted_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ DEFAULT now(),
    UNIQUE (store_id, email)
);

-- Index for token lookups
CREATE INDEX idx_store_invitations_token ON store_invitations(token);

-- Index for listing invitations by store
CREATE INDEX idx_store_invitations_store_id ON store_invitations(store_id);
