-- Deleting a comment on Instagram used to be mirrored locally as hidden=true,
-- because hidden already excluded the row from the private-reply lookup. That
-- overload made the UI render deleted comments as "oculto" (struck through,
-- unhide button) instead of removing them — the opposite of what Instagram
-- shows. Track deletion on its own column so the two states stay distinct.
ALTER TABLE live_comments ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- The comment list and the resend lookup both filter on this, always scoped by
-- event, so a partial index on the live rows keeps those reads cheap.
CREATE INDEX IF NOT EXISTS idx_live_comments_event_not_deleted
    ON live_comments (event_id, created_at)
    WHERE deleted_at IS NULL;
