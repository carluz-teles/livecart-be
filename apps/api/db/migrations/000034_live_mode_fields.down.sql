-- =============================================================================
-- LIVE MODE: Rollback - Remove active product and pause processing from live_events
-- =============================================================================

DROP INDEX IF EXISTS idx_live_events_active_product;
ALTER TABLE live_events DROP COLUMN IF EXISTS current_active_product_id;
ALTER TABLE live_events DROP COLUMN IF EXISTS processing_paused;
