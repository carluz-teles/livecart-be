-- =============================================================================
-- EVENT PRODUCTS (Whitelist)
-- Links products to events with per-event configuration
-- If empty, all store products are available (current behavior)
-- =============================================================================

CREATE TABLE event_products (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    special_price INTEGER,              -- Override price in cents (NULL = use product price)
    max_quantity INTEGER,               -- Max quantity for this product in this event (NULL = use default)
    display_order INTEGER NOT NULL DEFAULT 0,
    featured BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (event_id, product_id)
);

CREATE INDEX idx_event_products_event_id ON event_products(event_id);
CREATE INDEX idx_event_products_product_id ON event_products(product_id);
CREATE INDEX idx_event_products_display_order ON event_products(event_id, display_order);

COMMENT ON TABLE event_products IS 'Product whitelist for events. If empty, all store products are available.';
COMMENT ON COLUMN event_products.special_price IS 'Override price for this event in cents (NULL = use product default)';
COMMENT ON COLUMN event_products.max_quantity IS 'Max quantity per cart for this product in this event';
COMMENT ON COLUMN event_products.featured IS 'Highlighted product in live mode UI';

-- =============================================================================
-- EVENT UPSELLS (Global)
-- Upsell offers shown to all customers after adding any item
-- =============================================================================

CREATE TABLE event_upsells (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    discount_percent INTEGER NOT NULL DEFAULT 0 CHECK (discount_percent >= 0 AND discount_percent <= 100),
    message_template TEXT,              -- E.g., "Aproveite {product} com {discount}% OFF!"
    display_order INTEGER NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (event_id, product_id)
);

CREATE INDEX idx_event_upsells_event_id ON event_upsells(event_id);
CREATE INDEX idx_event_upsells_product_id ON event_upsells(product_id);
CREATE INDEX idx_event_upsells_active ON event_upsells(event_id, active) WHERE active = true;

COMMENT ON TABLE event_upsells IS 'Global upsell offers for events, shown after any cart addition.';
COMMENT ON COLUMN event_upsells.discount_percent IS 'Discount percentage (0-100)';
COMMENT ON COLUMN event_upsells.message_template IS 'Template with placeholders: {product}, {discount}, {price}';

-- =============================================================================
-- EVENT SCHEDULING & DESCRIPTION
-- =============================================================================

ALTER TABLE live_events
    ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS description TEXT;

COMMENT ON COLUMN live_events.scheduled_at IS 'When the event is scheduled to start (NULL = not scheduled)';
COMMENT ON COLUMN live_events.description IS 'Internal notes about the event';

-- Index for finding scheduled events
CREATE INDEX idx_live_events_scheduled_at ON live_events(scheduled_at) WHERE scheduled_at IS NOT NULL;
