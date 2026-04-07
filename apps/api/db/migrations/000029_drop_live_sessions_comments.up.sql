-- Drop deprecated `comments` JSONB column from live_sessions.
-- Replaced by the dedicated `live_comments` table introduced in migration 000026.
ALTER TABLE live_sessions DROP COLUMN IF EXISTS comments;
