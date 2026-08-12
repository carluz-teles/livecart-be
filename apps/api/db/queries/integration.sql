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
  AND provider IN ('tiny', 'mercado_pago', 'instagram')
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
