-- Integration Layer Migration
-- Adds encrypted credentials, idempotency support, and webhook event tracking

-- Step 1: Add new columns to integrations table
ALTER TABLE integrations
  ADD COLUMN IF NOT EXISTS credentials BYTEA,
  ADD COLUMN IF NOT EXISTS metadata JSONB;

-- Step 2: Migrate existing data from access_token/refresh_token to metadata (backup)
UPDATE integrations
SET metadata = jsonb_build_object(
  'migrated_access_token', access_token,
  'migrated_refresh_token', refresh_token,
  'migrated_at', now()
)
WHERE access_token IS NOT NULL OR refresh_token IS NOT NULL;

-- Step 3: Drop old columns
ALTER TABLE integrations
  DROP COLUMN IF EXISTS access_token,
  DROP COLUMN IF EXISTS refresh_token;

-- Step 4: Drop old extra_config if exists and different from metadata
ALTER TABLE integrations
  DROP COLUMN IF EXISTS extra_config;

-- Step 5: Add constraints for type, provider, and status
-- First, update any existing invalid values
UPDATE integrations SET type = 'payment' WHERE type NOT IN ('payment', 'erp');
UPDATE integrations SET provider = 'mercado_pago' WHERE provider NOT IN ('mercado_pago', 'tiny');
UPDATE integrations SET status = 'pending_auth' WHERE status NOT IN ('pending_auth', 'active', 'error', 'disconnected');

ALTER TABLE integrations
  DROP CONSTRAINT IF EXISTS integrations_type_check,
  DROP CONSTRAINT IF EXISTS integrations_provider_check,
  DROP CONSTRAINT IF EXISTS integrations_status_check;

ALTER TABLE integrations
  ADD CONSTRAINT integrations_type_check CHECK (type IN ('payment', 'erp')),
  ADD CONSTRAINT integrations_provider_check CHECK (provider IN ('mercado_pago', 'tiny')),
  ADD CONSTRAINT integrations_status_check CHECK (status IN ('pending_auth', 'active', 'error', 'disconnected'));

-- Step 6: Create idempotency_keys table
CREATE TABLE IF NOT EXISTS idempotency_keys (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  idempotency_key  VARCHAR(255) NOT NULL,
  store_id         UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
  integration_id   UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
  operation        VARCHAR(100) NOT NULL,
  request_hash     VARCHAR(64),
  response_payload JSONB,
  status           VARCHAR(20) NOT NULL DEFAULT 'pending',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '24 hours'),
  CONSTRAINT idempotency_keys_store_key_unique UNIQUE (store_id, idempotency_key),
  CONSTRAINT idempotency_keys_status_check CHECK (status IN ('pending', 'completed', 'failed'))
);

-- Indexes for idempotency lookups
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_hash
  ON idempotency_keys(store_id, request_hash, created_at)
  WHERE status = 'completed';

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires
  ON idempotency_keys(expires_at);

-- Step 7: Create webhook_events table
CREATE TABLE IF NOT EXISTS webhook_events (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  integration_id   UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
  provider         VARCHAR(50) NOT NULL,
  event_type       VARCHAR(100) NOT NULL,
  event_id         VARCHAR(255),
  payload          JSONB NOT NULL,
  signature_valid  BOOLEAN,
  processed        BOOLEAN NOT NULL DEFAULT false,
  processed_at     TIMESTAMPTZ,
  error_message    TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT webhook_events_integration_event_unique UNIQUE (integration_id, event_id)
);

-- Index for unprocessed webhooks
CREATE INDEX IF NOT EXISTS idx_webhook_events_unprocessed
  ON webhook_events(integration_id, processed, created_at)
  WHERE processed = false;

-- Index for provider filtering
CREATE INDEX IF NOT EXISTS idx_webhook_events_provider
  ON webhook_events(provider, created_at);
