-- Rollback Integration Layer Migration

-- Step 1: Drop webhook_events table
DROP TABLE IF EXISTS webhook_events;

-- Step 2: Drop idempotency_keys table
DROP TABLE IF EXISTS idempotency_keys;

-- Step 3: Drop constraints from integrations
ALTER TABLE integrations
  DROP CONSTRAINT IF EXISTS integrations_type_check,
  DROP CONSTRAINT IF EXISTS integrations_provider_check,
  DROP CONSTRAINT IF EXISTS integrations_status_check;

-- Step 4: Add back old columns
ALTER TABLE integrations
  ADD COLUMN IF NOT EXISTS access_token TEXT,
  ADD COLUMN IF NOT EXISTS refresh_token TEXT,
  ADD COLUMN IF NOT EXISTS extra_config JSONB;

-- Step 5: Restore data from metadata backup if exists
UPDATE integrations
SET
  access_token = metadata->>'migrated_access_token',
  refresh_token = metadata->>'migrated_refresh_token',
  extra_config = metadata - 'migrated_access_token' - 'migrated_refresh_token' - 'migrated_at'
WHERE metadata ? 'migrated_access_token';

-- Step 6: Drop new columns
ALTER TABLE integrations
  DROP COLUMN IF EXISTS credentials,
  DROP COLUMN IF EXISTS metadata;
