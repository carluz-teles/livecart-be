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

-- name: UpdateIntegrationMetadata :exec
UPDATE integrations
SET metadata = $2
WHERE id = $1;

-- name: DeleteIntegration :exec
DELETE FROM integrations WHERE id = $1 AND store_id = $2;

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

-- =============================================================================
-- SUBSCRIPTIONS
-- =============================================================================

-- name: GetActiveSubscription :one
SELECT * FROM subscriptions
WHERE store_id = $1 AND status IN ('active', 'trialing')
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateSubscription :one
INSERT INTO subscriptions (store_id, integration_id, external_subscription_id, status, current_period_start, current_period_end)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpdateSubscriptionStatus :exec
UPDATE subscriptions
SET status = $2, cancelled_at = $3
WHERE id = $1;

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
