-- Revert: Remove comments JSONB from live_sessions
ALTER TABLE live_sessions DROP COLUMN IF EXISTS comments;
