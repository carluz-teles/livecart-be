-- Create table for multiple platform IDs per live session
CREATE TABLE live_session_platforms (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id UUID NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
    platform VARCHAR NOT NULL,
    platform_live_id VARCHAR NOT NULL,
    added_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE(platform_live_id)  -- a platform live ID can only be in one session
);

CREATE INDEX idx_live_session_platforms_session_id ON live_session_platforms(session_id);
CREATE INDEX idx_live_session_platforms_platform_live_id ON live_session_platforms(platform_live_id);

-- Migrate existing data from live_sessions
INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
SELECT id, platform, platform_live_id
FROM live_sessions
WHERE platform_live_id IS NOT NULL AND platform_live_id != '';

-- Make platform_live_id nullable in live_sessions (kept for reference/primary)
ALTER TABLE live_sessions ALTER COLUMN platform_live_id DROP NOT NULL;
