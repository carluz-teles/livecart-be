-- Extend the integrations CHECK constraints so the table accepts the new
-- shipping integration (type=shipping, provider=melhor_envio).

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_type_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_type_check
    CHECK (type IN ('payment', 'erp', 'social', 'shipping'));

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram', 'melhor_envio'));
