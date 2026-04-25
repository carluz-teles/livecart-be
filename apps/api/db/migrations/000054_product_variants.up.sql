-- Product variants: adds a "product group" aggregator over the existing flat products table.
-- Each variant remains a row in `products` (keeping cart/stock-reservation/keyword logic intact);
-- a new `product_groups` table aggregates variants of the same conceptual product (e.g. "T-shirt"
-- with multiple Color × Size combinations).
--
-- Existing products keep working: they get group_id = NULL ("simple product, no variants").

-- 1) Aggregator
CREATE TABLE product_groups (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id        UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    name            VARCHAR(200) NOT NULL,
    description     TEXT,
    external_id     VARCHAR(100),
    external_source VARCHAR(20) NOT NULL DEFAULT 'manual',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_product_groups_store ON product_groups (store_id);

CREATE UNIQUE INDEX uq_product_groups_external
    ON product_groups (store_id, external_source, external_id)
    WHERE external_id IS NOT NULL;

-- 2) Options (e.g. "Color", "Size") and their values
CREATE TABLE product_options (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id  UUID NOT NULL REFERENCES product_groups(id) ON DELETE CASCADE,
    name      VARCHAR(50) NOT NULL,
    position  INT NOT NULL DEFAULT 0,
    UNIQUE (group_id, name)
);

CREATE INDEX idx_product_options_group ON product_options (group_id, position);

CREATE TABLE product_option_values (
    id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    option_id UUID NOT NULL REFERENCES product_options(id) ON DELETE CASCADE,
    value     VARCHAR(80) NOT NULL,
    position  INT NOT NULL DEFAULT 0,
    UNIQUE (option_id, value)
);

CREATE INDEX idx_product_option_values_option ON product_option_values (option_id, position);

-- 3) products → group + per-variant assignment of option values
ALTER TABLE products
    ADD COLUMN group_id UUID REFERENCES product_groups(id) ON DELETE SET NULL;

CREATE INDEX idx_products_group ON products (group_id);

CREATE TABLE product_variant_options (
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    option_value_id UUID NOT NULL REFERENCES product_option_values(id) ON DELETE CASCADE,
    PRIMARY KEY (product_id, option_value_id)
);

CREATE INDEX idx_product_variant_options_value ON product_variant_options (option_value_id);

-- 4) Image galleries (per-variant and per-group)
CREATE TABLE product_images (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    position   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_product_images_product ON product_images (product_id, position);

CREATE TABLE product_group_images (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id   UUID NOT NULL REFERENCES product_groups(id) ON DELETE CASCADE,
    url        TEXT NOT NULL,
    position   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_product_group_images_group ON product_group_images (group_id, position);

COMMENT ON TABLE  product_groups          IS 'Catalog aggregator over `products` (variants). NULL group_id = simple product.';
COMMENT ON TABLE  product_options         IS 'Variation dimensions of a group (e.g. Color, Size).';
COMMENT ON TABLE  product_option_values   IS 'Allowed values for a given option (e.g. Red, Blue / S, M, L).';
COMMENT ON TABLE  product_variant_options IS 'Junction: which option values define each variant. Uniqueness of the value combination per group is enforced in service layer.';
COMMENT ON TABLE  product_images          IS 'Per-variant gallery (in addition to products.image_url thumbnail).';
COMMENT ON TABLE  product_group_images    IS 'Per-group gallery (generic photos shared by all variants).';
COMMENT ON COLUMN products.group_id       IS 'FK to product_groups when this product is a variant; NULL for simple products.';
