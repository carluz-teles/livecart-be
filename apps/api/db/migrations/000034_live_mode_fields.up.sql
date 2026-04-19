-- =============================================================================
-- LIVE MODE: Add active product and pause processing to live_events
-- =============================================================================

-- Add current_active_product_id to live_events (FK to products)
ALTER TABLE live_events
ADD COLUMN current_active_product_id UUID REFERENCES products(id) ON DELETE SET NULL;

-- Add processing_paused boolean to live_events (default false)
ALTER TABLE live_events
ADD COLUMN processing_paused BOOLEAN NOT NULL DEFAULT false;

-- Create index for quick lookup of active product
CREATE INDEX idx_live_events_active_product ON live_events(current_active_product_id) WHERE current_active_product_id IS NOT NULL;

COMMENT ON COLUMN live_events.current_active_product_id IS 'Current highlighted product - used as fallback when user comments without keyword';
COMMENT ON COLUMN live_events.processing_paused IS 'When true, comments are stored but not processed into carts';
