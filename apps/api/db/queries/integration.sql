-- name: CreateIntegration :one
INSERT INTO integrations (store_id, type, provider, status, access_token, refresh_token, token_expires_at, extra_config)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetIntegrationByID :one
SELECT * FROM integrations WHERE id = $1 AND store_id = $2;

-- name: ListIntegrationsByStore :many
SELECT * FROM integrations WHERE store_id = $1 ORDER BY created_at;

-- name: UpdateIntegrationTokens :one
UPDATE integrations
SET access_token = $2, refresh_token = $3, token_expires_at = $4, status = 'active', last_synced_at = now()
WHERE id = $1
RETURNING *;

-- name: CreateIntegrationLog :one
INSERT INTO integration_logs (integration_id, entity_type, entity_id, direction, status, request_payload, response_payload, error_message)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetActiveSubscription :one
SELECT * FROM subscriptions
WHERE store_id = $1 AND status IN ('active', 'trialing')
ORDER BY created_at DESC
LIMIT 1;

-- name: CreateSubscription :one
INSERT INTO subscriptions (store_id, integration_id, external_subscription_id, status, current_period_start, current_period_end)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetPlatformIntegration :one
SELECT * FROM integrations
WHERE store_id = $1 AND type = 'platform' AND provider = $2 AND status = 'active';
