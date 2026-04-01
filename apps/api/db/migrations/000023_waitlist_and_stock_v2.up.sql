-- =============================================================================
-- 000023: Live Comments, Waitlist, ERP Contacts, Cart Item Waitlisted Flag
-- (re-apply of 000022 content that was skipped on Railway)
-- =============================================================================

-- live_comments: persists ALL comments from live sessions
CREATE TABLE IF NOT EXISTS live_comments (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id       UUID NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
    event_id         UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    platform         VARCHAR NOT NULL,
    platform_comment_id VARCHAR,
    platform_user_id VARCHAR NOT NULL,
    platform_handle  VARCHAR NOT NULL,
    text             TEXT NOT NULL,
    has_purchase_intent BOOLEAN NOT NULL DEFAULT false,
    matched_product_id UUID REFERENCES products(id),
    matched_quantity INTEGER,
    result           VARCHAR,
    created_at       TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_live_comments_session ON live_comments(session_id);
CREATE INDEX IF NOT EXISTS idx_live_comments_event ON live_comments(event_id);
CREATE INDEX IF NOT EXISTS idx_live_comments_event_user ON live_comments(event_id, platform_user_id);

-- waitlist_items: queue for users wanting out-of-stock products
CREATE TABLE IF NOT EXISTS waitlist_items (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id         UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    product_id       UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    platform_user_id VARCHAR NOT NULL,
    platform_handle  VARCHAR NOT NULL,
    quantity         INTEGER NOT NULL DEFAULT 1,
    position         INTEGER NOT NULL,
    status           VARCHAR NOT NULL DEFAULT 'waiting',
    notified_at      TIMESTAMPTZ,
    fulfilled_at     TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_waitlist_event_product ON waitlist_items(event_id, product_id, status);
CREATE INDEX IF NOT EXISTS idx_waitlist_event_user ON waitlist_items(event_id, platform_user_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_waitlist_position ON waitlist_items(event_id, product_id, position);

-- erp_contacts: cache of ERP contact IDs per platform user per store
CREATE TABLE IF NOT EXISTS erp_contacts (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id            UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    integration_id      UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    platform_user_id    VARCHAR NOT NULL,
    platform_handle     VARCHAR NOT NULL,
    external_contact_id VARCHAR NOT NULL,
    created_at          TIMESTAMPTZ DEFAULT now(),
    updated_at          TIMESTAMPTZ DEFAULT now(),
    UNIQUE(store_id, integration_id, platform_user_id)
);

-- cart_items: add waitlisted flag (idempotent)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'cart_items' AND column_name = 'waitlisted'
    ) THEN
        ALTER TABLE cart_items ADD COLUMN waitlisted BOOLEAN NOT NULL DEFAULT false;
    END IF;
END $$;

-- stores: add waitlist configuration (idempotent)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'stores' AND column_name = 'waitlist_claim_minutes'
    ) THEN
        ALTER TABLE stores ADD COLUMN waitlist_claim_minutes INTEGER DEFAULT 5;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'stores' AND column_name = 'waitlist_expiry_minutes'
    ) THEN
        ALTER TABLE stores ADD COLUMN waitlist_expiry_minutes INTEGER DEFAULT 30;
    END IF;
END $$;
