-- Default package dimensions per store. Used as a fallback when an ERP-imported
-- product (Tiny tipo=S or a variation) only ships with weight but no
-- height/width/length — common for clothing where merchants weigh items but
-- never measure the box. When all three defaults are set, the import flow
-- completes the shipping profile so the product becomes shippable without a
-- manual edit.
--
-- All three columns are nullable — when ANY is NULL the fallback is disabled
-- and the product lands without a shipping profile (current behavior).

ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS default_height_cm INTEGER,
    ADD COLUMN IF NOT EXISTS default_width_cm  INTEGER,
    ADD COLUMN IF NOT EXISTS default_length_cm INTEGER;

ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_height_cm_positive;
ALTER TABLE stores
    ADD CONSTRAINT stores_default_height_cm_positive
    CHECK (default_height_cm IS NULL OR default_height_cm > 0);

ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_width_cm_positive;
ALTER TABLE stores
    ADD CONSTRAINT stores_default_width_cm_positive
    CHECK (default_width_cm IS NULL OR default_width_cm > 0);

ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_length_cm_positive;
ALTER TABLE stores
    ADD CONSTRAINT stores_default_length_cm_positive
    CHECK (default_length_cm IS NULL OR default_length_cm > 0);

COMMENT ON COLUMN stores.default_height_cm IS 'Default package height (cm) for ERP imports that only carry weight. Combined with default_width_cm and default_length_cm.';
COMMENT ON COLUMN stores.default_width_cm  IS 'Default package width (cm) for ERP imports that only carry weight.';
COMMENT ON COLUMN stores.default_length_cm IS 'Default package length (cm) for ERP imports that only carry weight.';
