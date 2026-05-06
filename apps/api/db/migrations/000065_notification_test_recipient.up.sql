-- Test recipient for "Testar notificação" feature in the dashboard.
-- Lojista configures by sending a magic code from their personal IG to the
-- store's business account. The webhook handler matches the code and stores
-- the sender's IG-scoped ID + handle so future test sends know where to go.
-- The setup_code column is short-lived (10 min TTL controlled at app level).

ALTER TABLE stores
  ADD COLUMN notification_test_recipient_psid    TEXT,
  ADD COLUMN notification_test_recipient_handle  TEXT,
  ADD COLUMN notification_test_setup_code        TEXT,
  ADD COLUMN notification_test_setup_expires_at  TIMESTAMPTZ;
