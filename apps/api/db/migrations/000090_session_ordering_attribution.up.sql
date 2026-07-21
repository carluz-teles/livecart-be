-- Session ordering + per-session cart attribution (LIV-83).
-- The event→session model is already 1:N; this only ADDS ordering and a
-- first-touch session reference on cart items. Purely additive, reversible.

-- 1. Ordered sessions within an event (1st story -> 2nd live -> ...).
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS sequence_order INT NOT NULL DEFAULT 0;

-- Backfill existing rows: contiguous order per event by creation time.
UPDATE live_sessions ls
SET sequence_order = sub.rn
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY created_at, id) AS rn
    FROM live_sessions
) sub
WHERE ls.id = sub.id AND ls.sequence_order = 0;

-- Enforce unique, contiguous ordering per event. New rows compute MAX+1 in the
-- CreateLiveSession query, so the default 0 is never persisted for real inserts.
CREATE UNIQUE INDEX IF NOT EXISTS uq_live_sessions_event_sequence
    ON live_sessions (event_id, sequence_order);

-- 2. Per-session attribution: the session an item was first added in.
-- Revenue/items per session derive from this (first-touch: re-adds of the same
-- product in later sessions accumulate on the original row, keeping the first
-- session). ON DELETE SET NULL so deleting a session never drops cart items.
ALTER TABLE cart_items ADD COLUMN IF NOT EXISTS session_id UUID
    REFERENCES live_sessions(id) ON DELETE SET NULL;

-- Backfill from the cart's originating session (best-effort for historical data).
UPDATE cart_items ci
SET session_id = c.session_id
FROM carts c
WHERE ci.cart_id = c.id AND c.session_id IS NOT NULL AND ci.session_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_cart_items_session_id ON cart_items (session_id);
