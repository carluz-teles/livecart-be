-- =============================================================================
-- NOTIFICATION LOGS
-- =============================================================================

-- name: CreateNotificationLog :one
INSERT INTO notification_logs (
    store_id, event_id, cart_id, platform_user_id, platform_handle,
    notification_type, channel, status, message_text
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: CreateEmailNotificationLog :exec
-- Trilha unificada de auditoria de e-mails (lib/email): uma linha por
-- tentativa de envio (sent/failed/skipped), na mesma tabela dos DMs/WhatsApp.
-- platform_user_id carrega o e-mail do destinatário (identidade no canal).
INSERT INTO notification_logs (
    store_id, event_id, cart_id, platform_user_id,
    notification_type, channel, status, message_text,
    error_message, provider_message_id, sent_at
)
VALUES ($1, $2, $3, $4, $5, 'email', $6, $7, $8, $9, $10);

-- name: UpdateNotificationLogStatus :exec
UPDATE notification_logs
SET status = $2, sent_at = $3, error_message = $4
WHERE id = $1;

-- name: MarkNotificationUndelivered :exec
-- RN-38 — nem tentamos, porque a regra do Instagram ja fechava a porta.
-- sent_at fica NULL de proposito: nada saiu, e carimbar um horario de envio
-- aqui seria exatamente a ilusao de entrega que a regra proibe.
UPDATE notification_logs
SET status = 'undelivered',
    undelivered_reason = $2
WHERE id = $1;

-- name: SetNotificationUndeliveredReason :exec
-- RN-38 — tentamos e o Instagram recusou. O status continua 'failed' (foi
-- tentativa real, e o vocabulario de retry depende disso); o que a coluna
-- acrescenta e o motivo legivel, para a linha aparecer na lista do lojista
-- junto com as que nunca foram tentadas.
UPDATE notification_logs
SET undelivered_reason = $2
WHERE id = $1;

-- name: ListUndeliveredByEvent :many
-- RN-38 — os compradores que nao puderam ser avisados nesta campanha, um por
-- linha, com o carrinho e o link para o lojista chamar na mao.
--
-- O filtro e undelivered_reason IS NOT NULL, e nao status = 'undelivered', de
-- proposito: a lista tem de conter tambem o que foi TENTADO e recusado pelo
-- Instagram (status 'failed' + motivo). O status continua distinguindo
-- "nem tentamos" de "tentamos e recusaram"; quem responde "quem eu preciso
-- chamar na mao" e a coluna de motivo.
--
-- DISTINCT ON por comprador: uma campanha longa pode ter varias mensagens nao
-- entregues para a MESMA pessoa (o prazo do Instagram fechou para ela, entao
-- fecha para tudo). Listar cada tentativa transformaria a lista de "quem
-- chamar" numa lista de log — o lojista precisa de pessoas, nao de eventos.
SELECT DISTINCT ON (nl.platform_user_id)
    nl.id,
    nl.platform_user_id,
    nl.platform_handle,
    nl.notification_type,
    nl.undelivered_reason,
    nl.created_at,
    nl.cart_id,
    c.token AS cart_token,
    COALESCE(cart_product_total_cents(c.id), 0)::bigint AS cart_total_cents,
    COALESCE((SELECT SUM(ci.quantity)::int FROM cart_items ci WHERE ci.cart_id = c.id), 0)::int AS cart_total_items
FROM notification_logs nl
LEFT JOIN carts c ON c.id = nl.cart_id
WHERE nl.store_id = $1
  AND nl.event_id = $2
  AND nl.undelivered_reason IS NOT NULL
ORDER BY nl.platform_user_id, nl.created_at DESC;

-- name: GetLastNotificationForUser :one
-- Returns the most recent notification for a user in a store (for cooldown check)
SELECT * FROM notification_logs
WHERE store_id = $1 AND platform_user_id = $2 AND status = 'sent'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetNotificationByCartAndType :one
-- Check if a notification of this type was already sent for this cart
SELECT * FROM notification_logs
WHERE cart_id = $1 AND notification_type = $2 AND status = 'sent'
LIMIT 1;

-- name: ListNotificationsByEvent :many
-- List all notifications for an event (for analytics)
SELECT * FROM notification_logs
WHERE event_id = $1
ORDER BY created_at DESC;

-- name: ListNotificationsByStore :many
-- List recent notifications for a store
SELECT * FROM notification_logs
WHERE store_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: CountNotificationsByStatus :one
-- Count notifications by status for a store
SELECT
    COUNT(*) FILTER (WHERE status = 'sent')::int AS sent,
    COUNT(*) FILTER (WHERE status = 'failed')::int AS failed,
    COUNT(*) FILTER (WHERE status = 'cooldown')::int AS cooldown_skipped
FROM notification_logs
WHERE store_id = $1 AND created_at > $2;

-- =============================================================================
-- STORE NOTIFICATION SETTINGS
-- =============================================================================

-- name: GetStoreNotificationSettings :one
SELECT notification_settings FROM stores WHERE id = $1;

-- name: UpdateStoreNotificationSettings :exec
UPDATE stores SET notification_settings = $2 WHERE id = $1;

-- name: GetStoreCartMessageSettings :one
-- Returns cart message settings for notification triggers
SELECT
    cart_real_time,
    cart_message_cooldown_seconds,
    cart_send_expiration_reminder,
    cart_expiration_reminder_minutes
FROM stores WHERE id = $1;

-- =============================================================================
-- TEST RECIPIENT (for "Testar notificação" feature)
-- =============================================================================

-- name: GetStoreTestRecipient :one
-- Returns the configured test recipient and any active setup code for the store.
SELECT
    notification_test_recipient_psid,
    notification_test_recipient_handle,
    notification_test_setup_code,
    notification_test_setup_expires_at
FROM stores WHERE id = $1;

-- name: SetStoreTestSetupCode :exec
-- Persists a freshly generated setup code with a TTL. Called when the lojista
-- starts the "Configurar destinatário de teste" flow in the dashboard.
UPDATE stores
SET notification_test_setup_code = $2,
    notification_test_setup_expires_at = $3
WHERE id = $1;

-- name: SetStoreTestRecipient :exec
-- Stores the captured PSID + handle and clears the setup code. Called from the
-- IG webhook when an incoming DM matches the active setup code.
UPDATE stores
SET notification_test_recipient_psid = $2,
    notification_test_recipient_handle = $3,
    notification_test_setup_code = NULL,
    notification_test_setup_expires_at = NULL
WHERE id = $1;

-- name: FindStoreByActiveTestSetupCode :one
-- Looks up the store that owns a non-expired setup code. Used by the IG
-- webhook handler to route an incoming DM to the right store.
SELECT id
FROM stores
WHERE notification_test_setup_code = $1
  AND notification_test_setup_expires_at IS NOT NULL
  AND notification_test_setup_expires_at > now()
LIMIT 1;

-- =============================================================================
-- WHATSAPP (PRD 006)
-- =============================================================================

-- name: SetNotificationLogProviderMessageID :exec
-- Stamps the provider message SID right after a successful send so status
-- callbacks can be correlated back to this row.
UPDATE notification_logs
SET provider_message_id = $2
WHERE id = $1;

-- name: UpdateNotificationLogByProviderMessageID :one
-- Twilio status callbacks (sent/delivered/read/failed) arrive keyed by
-- MessageSid. sent_at is stamped once and preserved on later transitions.
UPDATE notification_logs
SET status = $2,
    error_message = $3,
    sent_at = COALESCE(sent_at, $4)
WHERE provider_message_id = $1
RETURNING *;

