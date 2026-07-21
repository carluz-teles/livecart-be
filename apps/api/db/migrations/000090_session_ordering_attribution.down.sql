DROP INDEX IF EXISTS idx_cart_items_session_id;
ALTER TABLE cart_items DROP COLUMN IF EXISTS session_id;
DROP INDEX IF EXISTS uq_live_sessions_event_sequence;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS sequence_order;
