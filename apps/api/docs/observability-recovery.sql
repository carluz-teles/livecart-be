-- Executar somente depois das migrações 149–152. Não modifica dados.
BEGIN READ ONLY;
SET LOCAL statement_timeout = '10s';

-- Global: todos os comentários sob o protocolo novo, sem texto ou dados pessoais.
SELECT count(*) FILTER (WHERE completed_at IS NULL) AS pendentes,
       count(*) FILTER (WHERE completed_at IS NULL AND lease_until > now()) AS em_execucao,
       count(*) FILTER (WHERE completed_at IS NULL AND next_attempt_at <= now()
                        AND (lease_until IS NULL OR lease_until < now())) AS prontos_para_tentar,
       max(now()-created_at) FILTER (WHERE completed_at IS NULL) AS idade_mais_antigo
FROM live_comment_work;

-- Pendências de itens por loja. Histórico não versionado fica fora da recuperação.
SELECT e.store_id,
       count(DISTINCT c.id) AS carrinhos_pendentes,
       count(*) FILTER (WHERE ci.erp_confirmed_quantity IS NOT NULL) AS linhas_versionadas,
       count(*) FILTER (WHERE ci.erp_confirmed_quantity IS NULL) AS linhas_historicas,
       max(now()-ci.erp_pending_since) AS idade_mais_antiga
FROM cart_items ci JOIN carts c ON c.id=ci.cart_id JOIN live_events e ON e.id=c.event_id
WHERE ci.erp_pending_since IS NOT NULL AND c.status NOT IN ('cancelled','expired')
GROUP BY e.store_id;

SELECT e.store_id, count(*) AS carrinhos_com_pagamento_em_conferencia
FROM carts c JOIN live_events e ON e.id=c.event_id
WHERE c.payment_review_required
GROUP BY e.store_id;

SELECT version, dirty FROM schema_migrations;
ROLLBACK;
