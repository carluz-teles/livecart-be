-- =============================================================================
-- ADD session_id TO CARTS
-- =============================================================================
-- Allows tracking which session created each cart while keeping event_id for persistence

-- 1. Add session_id column (nullable - cart can exist without active session)
ALTER TABLE carts ADD COLUMN session_id UUID REFERENCES live_sessions(id) ON DELETE SET NULL;

-- 2. Create index for querying carts by session
CREATE INDEX idx_carts_session_id ON carts(session_id);

COMMENT ON COLUMN carts.session_id IS 'Session where this cart was created. NULL if created outside a session or session was deleted.';
