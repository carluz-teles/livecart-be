DROP INDEX IF EXISTS idx_live_events_media_id;
ALTER TABLE live_events DROP COLUMN IF EXISTS webhook_active;
ALTER TABLE live_events DROP COLUMN IF EXISTS media_caption;
ALTER TABLE live_events DROP COLUMN IF EXISTS media_thumbnail_url;
ALTER TABLE live_events DROP COLUMN IF EXISTS media_permalink;
ALTER TABLE live_events DROP COLUMN IF EXISTS media_id;
