DROP INDEX IF EXISTS idx_integrations_store_payment_priority;
ALTER TABLE integrations DROP COLUMN IF EXISTS priority;
