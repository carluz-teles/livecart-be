-- Remove type field from live_events
DROP INDEX IF EXISTS idx_live_events_type;
ALTER TABLE live_events DROP COLUMN IF EXISTS type;
