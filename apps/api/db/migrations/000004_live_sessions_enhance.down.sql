-- Remove title and timestamps from live_sessions
ALTER TABLE live_sessions DROP COLUMN IF EXISTS title;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS created_at;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS updated_at;
