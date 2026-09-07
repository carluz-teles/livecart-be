ALTER TABLE notification_logs ADD COLUMN platform_comment_id text;
CREATE INDEX notification_logs_sent_comment ON notification_logs(store_id,platform_comment_id,notification_type) WHERE status='sent';
