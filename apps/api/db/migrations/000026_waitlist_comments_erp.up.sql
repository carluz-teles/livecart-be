-- =============================================================================
-- Add waitlisted column to cart_items
-- =============================================================================

ALTER TABLE cart_items ADD COLUMN IF NOT EXISTS waitlisted BOOLEAN DEFAULT false;

-- =============================================================================
-- Waitlist Items - Users waiting for out-of-stock products
-- =============================================================================

CREATE TABLE IF NOT EXISTS waitlist_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    platform_user_id VARCHAR NOT NULL,
    platform_handle VARCHAR NOT NULL,
    quantity INT NOT NULL DEFAULT 1,
    position INT NOT NULL,
    status VARCHAR NOT NULL DEFAULT 'waiting', -- waiting, notified, fulfilled, expired
    notified_at TIMESTAMPTZ,
    fulfilled_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_waitlist_event_product ON waitlist_items(event_id, product_id);
CREATE INDEX IF NOT EXISTS idx_waitlist_status ON waitlist_items(status);

-- =============================================================================
-- Live Comments - All comments from live sessions (for analytics and debugging)
-- =============================================================================

CREATE TABLE IF NOT EXISTS live_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id UUID REFERENCES live_sessions(id) ON DELETE SET NULL,
    event_id UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    platform VARCHAR NOT NULL DEFAULT 'instagram',
    platform_comment_id VARCHAR NOT NULL,
    platform_user_id VARCHAR NOT NULL,
    platform_handle VARCHAR NOT NULL,
    text TEXT NOT NULL,
    has_purchase_intent BOOLEAN DEFAULT false,
    matched_product_id UUID REFERENCES products(id) ON DELETE SET NULL,
    matched_quantity INT,
    result VARCHAR, -- no_intent, no_product, added_to_cart, waitlisted, already_waitlisted
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_comments_session ON live_comments(session_id);
CREATE INDEX IF NOT EXISTS idx_comments_event ON live_comments(event_id);
CREATE INDEX IF NOT EXISTS idx_comments_platform_id ON live_comments(platform_comment_id);

-- =============================================================================
-- ERP Contacts - Cache of contact IDs from external ERPs (like Tiny)
-- =============================================================================

CREATE TABLE IF NOT EXISTS erp_contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    integration_id UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    platform_user_id VARCHAR NOT NULL,
    platform_handle VARCHAR NOT NULL,
    external_contact_id VARCHAR NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (store_id, integration_id, platform_user_id)
);

CREATE INDEX IF NOT EXISTS idx_erp_contacts_store ON erp_contacts(store_id);
