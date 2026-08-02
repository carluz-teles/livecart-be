-- Reversível: nada foi removido de live_events, então a origem do backfill
-- continua intacta e o rollback só descarta cópias.
DROP INDEX IF EXISTS idx_lsp_webhook_pending;
ALTER TABLE live_session_platforms
    DROP COLUMN IF EXISTS media_permalink,
    DROP COLUMN IF EXISTS media_thumbnail_url,
    DROP COLUMN IF EXISTS media_caption,
    DROP COLUMN IF EXISTS webhook_active;

DROP INDEX IF EXISTS idx_live_sessions_type;
ALTER TABLE live_sessions DROP CONSTRAINT IF EXISTS live_sessions_type_check;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS type;
