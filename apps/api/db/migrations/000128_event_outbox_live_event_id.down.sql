DROP INDEX IF EXISTS idx_event_outbox_live_event;
ALTER TABLE event_outbox DROP COLUMN IF EXISTS live_event_id;
