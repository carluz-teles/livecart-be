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

-- name: GetWhatsAppRecoveryStats :one
-- PRD 006: last-30-days recovery funnel. A cart counts as recovered when it
-- was paid within 48h of the recovery message (same attribution window as
-- PRD 005).
-- Grupo A: revenue_recovered_cents reads from sealed orders (paid-only was already the intent).
SELECT
  COUNT(*) FILTER (WHERE nl.status IN ('sent', 'delivered', 'read'))::int AS messages_sent,
  COUNT(*) FILTER (
    WHERE c.payment_status = 'paid'
      AND c.paid_at > nl.created_at
      AND c.paid_at < nl.created_at + INTERVAL '48 hours'
  )::int AS carts_recovered,
  COALESCE(SUM(
    CASE WHEN c.payment_status = 'paid'
      AND c.paid_at > nl.created_at
      AND c.paid_at < nl.created_at + INTERVAL '48 hours'
    THEN o.total_cents
    ELSE 0 END
  ), 0)::bigint AS revenue_recovered_cents
FROM notification_logs nl
JOIN carts c ON c.id = nl.cart_id
LEFT JOIN orders o ON o.cart_id = c.id AND o.status = 'paid'
WHERE nl.store_id = $1
  AND nl.notification_type = 'cart_recovery'
  AND nl.created_at > NOW() - INTERVAL '30 days';
