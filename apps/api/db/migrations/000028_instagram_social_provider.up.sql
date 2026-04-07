-- Add Instagram provider and social type to integrations table

-- Update provider check constraint to include instagram
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_provider_check
  CHECK (provider IN ('mercado_pago', 'tiny', 'instagram'));

-- Update type check constraint to include social
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_type_check
  CHECK (type IN ('payment', 'erp', 'social'));
