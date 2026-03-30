-- =============================================================================
-- RESTRUCTURE: LiveEvent -> LiveSession -> LiveSessionPlatforms
-- =============================================================================

-- 1. Create live_events table
CREATE TABLE live_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    title VARCHAR,
    status VARCHAR NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_live_events_store_id ON live_events(store_id);
CREATE INDEX idx_live_events_status ON live_events(status);

COMMENT ON TABLE live_events IS 'Container for live sessions. Carts are tied to events, not sessions.';
COMMENT ON COLUMN live_events.status IS 'active = event ongoing, ended = checkout finalized';

-- 2. Migrate existing live_sessions to live_events (1 event per session)
INSERT INTO live_events (id, store_id, title, status, created_at, updated_at)
SELECT
    id,  -- use same ID for easy migration
    store_id,
    title,
    CASE WHEN status = 'ended' THEN 'ended' ELSE 'active' END,
    created_at,
    updated_at
FROM live_sessions;

-- 3. Add event_id to live_sessions
ALTER TABLE live_sessions ADD COLUMN event_id UUID REFERENCES live_events(id) ON DELETE CASCADE;

-- 4. Set event_id = id (since we used same ID for the event)
UPDATE live_sessions SET event_id = id;

-- 5. Make event_id NOT NULL
ALTER TABLE live_sessions ALTER COLUMN event_id SET NOT NULL;

-- 6. Add platform to live_session_platforms if not exists (should already exist)
-- Already has: platform VARCHAR NOT NULL

-- 7. Migrate platform from live_sessions to live_session_platforms (for records not yet migrated)
-- The migration 000018 already did this, but let's ensure any new records are covered
INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
SELECT id, platform, platform_live_id
FROM live_sessions
WHERE platform_live_id IS NOT NULL
  AND platform_live_id != ''
  AND id NOT IN (SELECT session_id FROM live_session_platforms);

-- 8. Update carts: rename session_id to event_id
ALTER TABLE carts RENAME COLUMN session_id TO event_id;

-- 9. Update foreign key constraint on carts
ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_session_id_fkey;
ALTER TABLE carts ADD CONSTRAINT carts_event_id_fkey
    FOREIGN KEY (event_id) REFERENCES live_events(id) ON DELETE CASCADE;

-- 10. Move total_orders from live_sessions to live_events
ALTER TABLE live_events ADD COLUMN total_orders INT NOT NULL DEFAULT 0;

UPDATE live_events e
SET total_orders = COALESCE((SELECT total_orders FROM live_sessions s WHERE s.id = e.id), 0);

-- 11. Drop columns from live_sessions that moved to live_events or live_session_platforms
ALTER TABLE live_sessions DROP COLUMN IF EXISTS store_id;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS title;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS platform;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS platform_live_id;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS total_orders;

-- 12. Create index on live_sessions.event_id
CREATE INDEX idx_live_sessions_event_id ON live_sessions(event_id);
