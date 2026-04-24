-- Extend the integrations provider check constraint to accept the new
-- SmartEnvios shipping provider.

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram', 'melhor_envio', 'smartenvios'));
