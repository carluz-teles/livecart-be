-- Add shipping-related fields to products so we can quote freight via Melhor Envio.
-- All columns are nullable; they only become required when the store activates the shipping integration.

ALTER TABLE products
    ADD COLUMN IF NOT EXISTS weight_grams          INTEGER,
    ADD COLUMN IF NOT EXISTS height_cm             INTEGER,
    ADD COLUMN IF NOT EXISTS width_cm              INTEGER,
    ADD COLUMN IF NOT EXISTS length_cm             INTEGER,
    ADD COLUMN IF NOT EXISTS sku                   VARCHAR(100),
    ADD COLUMN IF NOT EXISTS package_format        VARCHAR(10) NOT NULL DEFAULT 'box',
    ADD COLUMN IF NOT EXISTS insurance_value_cents BIGINT;

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_package_format_check;

ALTER TABLE products
    ADD CONSTRAINT products_package_format_check
    CHECK (package_format IN ('box', 'roll', 'letter'));

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_weight_grams_positive;
ALTER TABLE products
    ADD CONSTRAINT products_weight_grams_positive CHECK (weight_grams IS NULL OR weight_grams > 0);

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_height_cm_positive;
ALTER TABLE products
    ADD CONSTRAINT products_height_cm_positive CHECK (height_cm IS NULL OR height_cm > 0);

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_width_cm_positive;
ALTER TABLE products
    ADD CONSTRAINT products_width_cm_positive CHECK (width_cm IS NULL OR width_cm > 0);

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_length_cm_positive;
ALTER TABLE products
    ADD CONSTRAINT products_length_cm_positive CHECK (length_cm IS NULL OR length_cm > 0);

CREATE INDEX IF NOT EXISTS idx_products_sku ON products(store_id, sku) WHERE sku IS NOT NULL;

COMMENT ON COLUMN products.weight_grams IS 'Product weight in grams (packaged individually). Required to quote shipping.';
COMMENT ON COLUMN products.height_cm IS 'Product height in cm (packaged individually). Required to quote shipping.';
COMMENT ON COLUMN products.width_cm IS 'Product width in cm (packaged individually). Required to quote shipping.';
COMMENT ON COLUMN products.length_cm IS 'Product length in cm (packaged individually). Required to quote shipping.';
COMMENT ON COLUMN products.sku IS 'Full SKU used by ERP and shipping labels (distinct from short keyword).';
COMMENT ON COLUMN products.package_format IS 'Shape hint for carriers: box (default), roll (tube), letter (envelope).';
COMMENT ON COLUMN products.insurance_value_cents IS 'Declared value for shipping insurance, in cents. Falls back to product price when NULL.';
