-- =============================================================================
-- ADD comments JSONB TO live_sessions
-- =============================================================================
-- Stores all comments received during the session as a JSONB array
-- Structure: [{"handle": "@user", "text": "quero 1"}, ...]

ALTER TABLE live_sessions ADD COLUMN comments JSONB DEFAULT '[]'::jsonb;

COMMENT ON COLUMN live_sessions.comments IS 'Array of comments: [{handle, text}]';
