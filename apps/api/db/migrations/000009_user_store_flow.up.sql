-- Flag de onboarding na store
ALTER TABLE stores
  ADD COLUMN IF NOT EXISTS onboarding_complete BOOLEAN NOT NULL DEFAULT false;

-- Índice para buscar store_users por clerk_user_id
CREATE INDEX IF NOT EXISTS idx_store_users_clerk_user_id ON store_users(clerk_user_id);
