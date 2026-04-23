DROP INDEX IF EXISTS idx_products_sku;

ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_package_format_check,
    DROP CONSTRAINT IF EXISTS products_weight_grams_positive,
    DROP CONSTRAINT IF EXISTS products_height_cm_positive,
    DROP CONSTRAINT IF EXISTS products_width_cm_positive,
    DROP CONSTRAINT IF EXISTS products_length_cm_positive;

ALTER TABLE products
    DROP COLUMN IF EXISTS weight_grams,
    DROP COLUMN IF EXISTS height_cm,
    DROP COLUMN IF EXISTS width_cm,
    DROP COLUMN IF EXISTS length_cm,
    DROP COLUMN IF EXISTS sku,
    DROP COLUMN IF EXISTS package_format,
    DROP COLUMN IF EXISTS insurance_value_cents;
