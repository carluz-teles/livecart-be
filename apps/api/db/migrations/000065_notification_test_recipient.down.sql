ALTER TABLE stores
  DROP COLUMN IF EXISTS notification_test_recipient_psid,
  DROP COLUMN IF EXISTS notification_test_recipient_handle,
  DROP COLUMN IF EXISTS notification_test_setup_code,
  DROP COLUMN IF EXISTS notification_test_setup_expires_at;
