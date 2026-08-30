-- =============================================================================
-- INTEGRATIONS
-- =============================================================================

-- name: CreateIntegration :one
INSERT INTO integrations (store_id, type, provider, status, credentials, token_expires_at, metadata)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE(NULLIF($7::text, '')::jsonb, '{}'::jsonb))
RETURNING *;

-- name: GetIntegrationByID :one
SELECT * FROM integrations WHERE id = $1 AND store_id = $2;

-- name: GetIntegrationByIDOnly :one
SELECT * FROM integrations WHERE id = $1;

-- name: ListIntegrationsByStore :many
SELECT * FROM integrations WHERE store_id = $1 ORDER BY created_at DESC;

-- name: ListIntegrationsByType :many
SELECT * FROM integrations
WHERE store_id = $1 AND type = $2 AND status = 'active'
ORDER BY created_at DESC;

-- name: GetActiveIntegrationByProvider :one
SELECT * FROM integrations
WHERE store_id = $1 AND type = $2 AND provider = $3 AND status = 'active'
LIMIT 1;

-- GetActiveERPIntegration resolve o ERP ATIVO da loja pela LINHA, sem o literal
-- do provider. É seguro devolver :one porque o índice parcial
-- uniq_integrations_store_one_erp (migration 000061) garante no máximo uma
-- integração de ERP por loja — a regra de negócio "ou Tiny ou Bling, nunca os
-- dois" é do banco, não da convenção de quem escreve a query.
--
-- Existe para substituir os call sites que perguntavam por provider='tiny'
-- literal: com Bling conectado, aqueles devolviam not-found e QUATRO deles
-- tratavam isso como "loja sem ERP", devolvendo nil em silêncio — a live rodava
-- inteira sem criar um pedido e sem uma linha de log.
-- name: GetActiveERPIntegration :one
SELECT * FROM integrations
WHERE store_id = $1 AND type = 'erp' AND status = 'active'
LIMIT 1;

-- GetActiveERPByAccount resolve a LOJA a partir da conta do ERP. É como o
-- webhook do Bling, que chega numa URL única para todas as lojas, descobre de
-- quem é o evento: o `companyId` do envelope casa com integrations.erp_account_id.
--
-- Usa o índice parcial idx_integrations_erp_account.
-- name: GetActiveERPByAccount :one
SELECT * FROM integrations
WHERE type = 'erp' AND provider = $1 AND erp_account_id = $2 AND status = 'active'
LIMIT 1;

-- SetIntegrationERPAccount grava a identidade da conta do ERP no fim do OAuth.
-- name: SetIntegrationERPAccount :exec
UPDATE integrations
SET erp_account_id = $2
WHERE id = $1;

-- name: GetIntegrationByProvider :one
SELECT * FROM integrations
WHERE store_id = $1 AND type = $2 AND provider = $3 AND status IN ('active', 'pending_auth')
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdateIntegrationCredentials :exec
UPDATE integrations
SET credentials = $2, token_expires_at = $3, status = 'active', last_synced_at = now()
WHERE id = $1;

-- name: UpdateIntegrationStatus :exec
UPDATE integrations
SET status = $2, last_synced_at = now()
WHERE id = $1;

-- name: HealIntegrationFromError :execrows
-- Devolve a integração de 'error' para 'active' quando uma chamada volta a dar
-- certo. Condicional de propósito: só toca a linha que ESTÁ em 'error', então
-- não pisa em 'pending_auth' (autorização em andamento) nem em 'disconnected'
-- (o lojista desligou de propósito).
--
-- Existe porque 'error' não tinha saída automática. Um HTTP 429 — que é
-- transitório por definição — marcava a integração como quebrada e nada nunca
-- revertia: o botão de sincronizar sumia do painel e só reconectar à mão
-- resolvia. Aconteceu em 09/08/2026 e deixou o ERP parado por três dias.
--
-- Não mexe em last_synced_at: quem acabou de rodar a operação é que carimba isso.
UPDATE integrations
SET status = 'active'
WHERE id = $1 AND status = 'error';

-- name: UpdateIntegrationMetadata :exec
UPDATE integrations
SET metadata = $2
WHERE id = $1;

-- name: UpdateIntegrationPriority :exec
-- Sets the priority of a single integration for an explicit store. Lower
-- number = higher priority in the checkout selection ordering.
UPDATE integrations
SET priority = $3
WHERE id = $1 AND store_id = $2;

-- name: DeleteIntegration :exec
DELETE FROM integrations WHERE id = $1 AND store_id = $2;

-- name: ListIntegrationsWithExpiringTokens :many
-- Lists active integrations with OAuth tokens expiring within the given duration.
-- Used by background token refresh worker.
SELECT * FROM integrations
WHERE status = 'active'
  AND token_expires_at IS NOT NULL
  AND token_expires_at <= $1
  -- ⚠ Acrescentar provider aqui é a mudança de UMA PALAVRA que pode derrubar a
  -- frota inteira. O Bling bloqueia o IP por 60 MINUTOS depois de 20 chamadas a
  -- /oauth/token em 60 s, e o IP é o NAT compartilhado do Railway — durante o
  -- bloqueio ninguém renova E ninguém consegue conectar.
  --
  -- Por isso 'bling' só entrou junto com o espaçamento no worker
  -- (TokenRefreshWorker.refreshExpiringTokens). Nunca acrescente um provider
  -- aqui sem conferir que o worker respeita o teto de chamadas dele.
  AND provider IN ('tiny', 'mercado_pago', 'instagram', 'bling')
ORDER BY token_expires_at ASC
LIMIT 100;

-- =============================================================================
-- INTEGRATION LOGS
-- =============================================================================

-- name: CreateIntegrationLog :one
INSERT INTO integration_logs (integration_id, entity_type, entity_id, direction, status, request_payload, response_payload, error_message)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListIntegrationLogs :many
SELECT * FROM integration_logs
WHERE integration_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountIntegrationLogs :one
SELECT COUNT(*) FROM integration_logs WHERE integration_id = $1;

-- =============================================================================
-- IDEMPOTENCY KEYS
-- =============================================================================

-- name: GetIdempotencyByKey :one
SELECT * FROM idempotency_keys
WHERE store_id = $1 AND idempotency_key = $2 AND expires_at > now();

-- name: GetIdempotencyByHash :one
SELECT * FROM idempotency_keys
WHERE store_id = $1 AND request_hash = $2 AND created_at > $3 AND status = 'completed'
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateIdempotencyKey :one
INSERT INTO idempotency_keys (idempotency_key, store_id, integration_id, operation, request_hash, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateIdempotencyKey :exec
UPDATE idempotency_keys
SET response_payload = $2, status = $3
WHERE id = $1;

-- name: ReclaimIdempotencyKey :execrows
-- Toma posse (CAS) de uma tentativa anterior: 'failed', ou 'pending' velha o
-- bastante para a tentativa dona ter morrido no meio (janela casa com
-- stalePendingAfter em lib/idempotency). 0 linhas = outra tentativa chegou
-- antes; o chamador vira ErrInFlight, nunca execução dupla.
UPDATE idempotency_keys
SET status = 'pending', response_payload = NULL,
    expires_at = now() + interval '24 hours'
WHERE id = $1
  AND (status = 'failed'
       OR (status = 'pending' AND created_at < now() - interval '5 minutes'));

-- name: DeleteExpiredIdempotencyKeys :exec
DELETE FROM idempotency_keys WHERE expires_at < now();

-- =============================================================================
-- WEBHOOK EVENTS
-- =============================================================================

-- name: CreateWebhookEvent :one
INSERT INTO webhook_events (integration_id, provider, event_type, event_id, payload, signature_valid)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetWebhookEventByEventID :one
SELECT * FROM webhook_events
WHERE integration_id = $1 AND event_id = $2;

-- name: MarkWebhookProcessed :exec
UPDATE webhook_events
SET processed = true, processed_at = now()
WHERE id = $1;

-- name: MarkWebhookFailed :exec
UPDATE webhook_events
SET processed = true, processed_at = now(), error_message = $2
WHERE id = $1;

-- name: ListUnprocessedWebhooks :many
SELECT * FROM webhook_events
WHERE integration_id = $1 AND processed = false
ORDER BY created_at ASC
LIMIT $2;

-- name: ListWebhookEvents :many
SELECT * FROM webhook_events
WHERE integration_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- (Queries de subscriptions migraram para db/queries/subscription.sql na
-- remodelagem do billing via Stripe — PRD 007, migration 000076.)

-- =============================================================================
-- OAUTH STATES (PKCE)
-- =============================================================================

-- name: CreateOAuthState :exec
INSERT INTO oauth_states (state, store_id, provider, code_verifier)
VALUES ($1, $2, $3, $4);

-- name: GetOAuthState :one
SELECT * FROM oauth_states
WHERE state = $1 AND expires_at > now();

-- ConsumeOAuthState valida e APAGA o state numa operação só.
--
-- Diferente do par GetOAuthState + DeleteOAuthState, que são duas queries e
-- deixam uma janela entre a leitura e o apagamento. Para o Bling isso não é
-- preciosismo: a doc avisa que reusar um authorization code ainda válido faz o
-- usuário ter "o seu acesso revogado por medidas de segurança". Um duplo clique
-- no callback com o par não-atômico passaria os dois pela validação.
-- name: ConsumeOAuthState :one
DELETE FROM oauth_states
WHERE state = $1 AND expires_at > now()
RETURNING *;

-- name: DeleteOAuthState :exec
DELETE FROM oauth_states WHERE state = $1;

-- name: DeleteExpiredOAuthStates :exec
DELETE FROM oauth_states WHERE expires_at < now();

-- name: ListTinyIntegrationsWithStaleStockWebhook :many
-- Health-check de ENTREGA de webhook (aprendizado de 11/07: o Tiny remove o
-- cadastro da URL após falhas consecutivas e para de entregar em silêncio).
-- Lista integrações Tiny ativas, criadas há mais de @stale_hours, sem NENHUM
-- webhook de estoque nesse período e ainda não alertadas nas últimas 24h
-- (dedupe via metadata->>'stock_webhook_alerted_at').
SELECT i.id, i.store_id, MAX(we.created_at)::timestamptz AS last_stock_event_at
FROM integrations i
LEFT JOIN webhook_events we
       ON we.integration_id = i.id AND we.event_type = 'estoque'
WHERE i.type = 'erp' AND i.provider = 'tiny' AND i.status = 'active'
  AND i.created_at < now() - make_interval(hours => sqlc.arg(stale_hours)::int)
  AND COALESCE((i.metadata->>'stock_webhook_alerted_at')::timestamptz, 'epoch'::timestamptz)
      < now() - interval '24 hours'
GROUP BY i.id, i.store_id
HAVING COALESCE(MAX(we.created_at), 'epoch'::timestamptz)
       < now() - make_interval(hours => sqlc.arg(stale_hours)::int);

-- name: StampIntegrationStockWebhookAlert :exec
-- Dedupe do alerta acima: carimba o momento no metadata (merge, não replace).
UPDATE integrations
SET metadata = COALESCE(metadata, '{}'::jsonb)
    || jsonb_build_object('stock_webhook_alerted_at', to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))
WHERE id = $1;
