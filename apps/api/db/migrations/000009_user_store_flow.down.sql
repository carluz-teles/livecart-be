ALTER TABLE stores DROP COLUMN IF EXISTS onboarding_complete;
DROP INDEX IF EXISTS idx_store_users_clerk_user_id;
