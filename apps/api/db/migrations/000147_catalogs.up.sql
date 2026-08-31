-- Catalogs: a named collection of products (e.g. "Catálogo de Páscoa", "Catálogo de Natal)
-- that can be reused across many live events. A catalog is only a display grouping —
-- it does NOT change which products are sellable in an event (that stays with
-- session_products / all active products). Cardinality: 1 catalog -> N events.

CREATE TABLE catalogs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id   UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_catalogs_store_id ON catalogs(store_id);

COMMENT ON TABLE catalogs IS 'Named, reusable collection of products used to group a live catalog for buyers. Display grouping only — does not affect sellable-product resolution.';

-- Membership: which products belong to a catalog. position drives display order.
CREATE TABLE catalog_products (
    catalog_id UUID NOT NULL REFERENCES catalogs(id) ON DELETE CASCADE,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    position   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (catalog_id, product_id)
);

CREATE INDEX idx_catalog_products_product_id ON catalog_products(product_id);

-- Event association: each event points to at most one catalog; a catalog serves many events.
ALTER TABLE live_events
    ADD COLUMN catalog_id UUID REFERENCES catalogs(id) ON DELETE SET NULL;

CREATE INDEX idx_live_events_catalog_id ON live_events(catalog_id);

COMMENT ON COLUMN live_events.catalog_id IS 'Optional catalog shown on the buyer catalog page for this event. NULL = no catalog associated.';
