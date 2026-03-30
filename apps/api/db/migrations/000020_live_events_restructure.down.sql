-- =============================================================================
-- ROLLBACK: LiveEvent -> LiveSession -> LiveSessionPlatforms
-- =============================================================================

-- 1. Re-add columns to live_sessions
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS store_id UUID;
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS title VARCHAR;
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS platform VARCHAR;
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS platform_live_id VARCHAR;
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS total_orders INT NOT NULL DEFAULT 0;

-- 2. Copy data back from live_events
UPDATE live_sessions s
SET
    store_id = e.store_id,
    title = e.title,
    total_orders = e.total_orders
FROM live_events e
WHERE s.event_id = e.id;

-- 3. Get platform data back from live_session_platforms (first platform per session)
UPDATE live_sessions s
SET
    platform = p.platform,
    platform_live_id = p.platform_live_id
FROM (
    SELECT DISTINCT ON (session_id) session_id, platform, platform_live_id
    FROM live_session_platforms
    ORDER BY session_id, created_at
) p
WHERE s.id = p.session_id;

-- 4. Make store_id NOT NULL
ALTER TABLE live_sessions ALTER COLUMN store_id SET NOT NULL;

-- 5. Add foreign key for store_id
ALTER TABLE live_sessions ADD CONSTRAINT live_sessions_store_id_fkey
    FOREIGN KEY (store_id) REFERENCES stores(id) ON DELETE CASCADE;

-- 6. Rename carts.event_id back to session_id
ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_event_id_fkey;
ALTER TABLE carts RENAME COLUMN event_id TO session_id;
ALTER TABLE carts ADD CONSTRAINT carts_session_id_fkey
    FOREIGN KEY (session_id) REFERENCES live_sessions(id) ON DELETE CASCADE;

-- 7. Drop event_id from live_sessions
ALTER TABLE live_sessions DROP COLUMN IF EXISTS event_id;

-- 8. Drop live_events table
DROP TABLE IF EXISTS live_events;
