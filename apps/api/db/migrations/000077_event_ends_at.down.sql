DROP INDEX IF EXISTS idx_live_events_ends_at;
ALTER TABLE live_events DROP COLUMN IF EXISTS ends_at;
