-- Rollback notification settings
DROP TABLE IF EXISTS notification_logs;
ALTER TABLE stores DROP COLUMN IF EXISTS notification_settings;
