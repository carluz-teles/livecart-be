DROP INDEX IF EXISTS idx_live_comments_event_not_deleted;
ALTER TABLE live_comments DROP COLUMN IF EXISTS deleted_at;
