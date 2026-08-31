-- Reverte a 000145. A ordem é a inversa da subida: índices primeiro (dependem
-- da coluna), depois a coluna, depois o CHECK.

DROP INDEX IF EXISTS uniq_integrations_erp_account;
DROP INDEX IF EXISTS idx_integrations_erp_account;

-- A coluna sai por último entre os artefatos que dependem dela. Descer isto
-- PERDE a identidade da conta de toda integração Bling — subir de novo exige
-- reconectar as lojas para repopular. É aceitável porque o CHECK abaixo também
-- rejeitaria essas linhas.
ALTER TABLE integrations DROP COLUMN IF EXISTS erp_account_id;

-- Volta a lista de providers da 000081, sem 'bling'.
--
-- ⚠ Se existir QUALQUER linha com provider='bling', este ADD CONSTRAINT falha e
-- a migration para — deliberadamente. Descer uma migration que deixaria linhas
-- órfãs violando o CHECK seria pior do que falhar em voz alta: desconecte as
-- integrações Bling antes de descer.
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram',
                        'melhor_envio', 'smartenvios', 'twilio_whatsapp'));
