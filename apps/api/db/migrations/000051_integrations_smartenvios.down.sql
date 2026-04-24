-- Revert the SmartEnvios addition: any smartenvios integrations must be
-- removed beforehand, otherwise the constraint re-creation will fail.

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram', 'melhor_envio'));
