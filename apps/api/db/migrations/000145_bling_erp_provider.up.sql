-- Bling entra como segundo provedor de ERP.
--
-- Regra de negócio (do Alisson, 29/08/2026): uma loja integra UM ERP — Tiny OU
-- Bling, nunca os dois. Isso JÁ é garantido desde a 000061 pelo índice parcial
-- uniq_integrations_store_one_erp sobre (store_id) WHERE type='erp'; esta
-- migration não afrouxa aquilo, só passa a admitir 'bling' como o valor do ERP
-- escolhido.

-- 1. O CHECK de provider aceita 'bling'.
--    Mantém a lista vigente da 000081 e acrescenta um valor — nenhuma remoção,
--    para que o down seja exatamente o inverso.
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_provider_check;
ALTER TABLE integrations
    ADD CONSTRAINT integrations_provider_check
    CHECK (provider IN ('mercado_pago', 'pagarme', 'tiny', 'instagram',
                        'melhor_envio', 'smartenvios', 'twilio_whatsapp', 'bling'));

-- 2. Identidade da CONTA do ERP, separada da identidade da integração.
--
-- Por que uma coluna e não uma chave no metadata: ela tem dois consumidores
-- quentes que precisam de índice, não de leitura de JSONB.
--
--   (a) O webhook do Bling chega numa URL ÚNICA para todas as lojas — o Bling
--       não tem API para registrar webhook por loja, a URL é do APLICATIVO. A
--       única coisa que identifica a origem é o `companyId` do envelope, e
--       MEDIDO em 29/08/2026 ele é byte-idêntico ao `data.id` de
--       GET /empresas/me/dados-basicos. É por ele que se resolve a loja.
--
--   (b) O teto de requisições do Bling é POR CONTA (3 req/s somando TODOS os
--       apps do lojista), não por integração. Duas lojas LiveCart ligadas à
--       MESMA empresa Bling dividem um teto só, e o limitador precisa saber
--       disso — chavear por integration_id daria dois baldes para uma cota.
--
-- Fica NULL para toda linha Tiny existente, e a chave de cota cai no
-- integration_id nesse caso: topologia idêntica à de hoje, zero migração de dado.
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS erp_account_id VARCHAR;

COMMENT ON COLUMN integrations.erp_account_id IS
    'Identificador da CONTA no ERP (Bling: data.id de /empresas/me/dados-basicos, '
    'que é o mesmo companyId do webhook). Chave de rate limit e de roteamento de '
    'webhook por URL única. NULL para Tiny, que não tem esse conceito.';

-- 3. Índice do caminho quente: resolver a loja a partir do companyId do webhook.
--    Parcial porque só linhas de ERP com a conta preenchida entram na busca.
CREATE INDEX IF NOT EXISTS idx_integrations_erp_account
    ON integrations (provider, erp_account_id)
    WHERE type = 'erp' AND erp_account_id IS NOT NULL;

-- 4. Duas lojas LiveCart na MESMA conta Bling dividiriam o teto de 3 req/s sem
--    saber uma da outra, e o webhook de URL única não teria como decidir para
--    qual das duas entregar o evento. O banco recusa a segunda.
--
--    Escopado a type='erp' e a erp_account_id NOT NULL, então nenhuma linha
--    Tiny (que tem a coluna NULL) é afetada.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_integrations_erp_account
    ON integrations (provider, erp_account_id)
    WHERE type = 'erp' AND erp_account_id IS NOT NULL AND status <> 'disconnected';
