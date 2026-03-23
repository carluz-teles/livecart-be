-- Add clerk_user_id to store_users
ALTER TABLE store_users ADD COLUMN clerk_user_id VARCHAR UNIQUE;

-- Create index for faster lookups
CREATE INDEX idx_store_users_clerk_user_id ON store_users(clerk_user_id);

-- Add name column for display
ALTER TABLE store_users ADD COLUMN name VARCHAR;

-- Add avatar_url column
ALTER TABLE store_users ADD COLUMN avatar_url TEXT;

-- Add updated_at column
ALTER TABLE store_users ADD COLUMN updated_at TIMESTAMPTZ DEFAULT now();
