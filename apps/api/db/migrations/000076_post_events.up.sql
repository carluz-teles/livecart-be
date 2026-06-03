-- Post-commerce events: map a published Instagram post/reel instead of a live.
-- Comments on the post are captured (polling first, then the `comments` webhook)
-- and processed into carts exactly like live comments, since the post's media id
-- is stored as the session platform_live_id.

ALTER TABLE live_events ADD COLUMN IF NOT EXISTS media_id VARCHAR;
ALTER TABLE live_events ADD COLUMN IF NOT EXISTS media_permalink VARCHAR;
ALTER TABLE live_events ADD COLUMN IF NOT EXISTS media_thumbnail_url VARCHAR;
ALTER TABLE live_events ADD COLUMN IF NOT EXISTS media_caption TEXT;
-- Tracks whether a real-time `comments` webhook has been seen for this event,
-- so the polling capture can stop once webhooks take over.
ALTER TABLE live_events ADD COLUMN IF NOT EXISTS webhook_active BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_live_events_media_id ON live_events(media_id);

COMMENT ON COLUMN live_events.media_id IS 'Instagram media id when type = post';
COMMENT ON COLUMN live_events.webhook_active IS 'true once a comments webhook arrived for this post event; polling stops';
