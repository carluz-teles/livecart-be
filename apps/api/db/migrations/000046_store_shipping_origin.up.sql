-- Extend the store address with fields required to ship with Brazilian carriers
-- (number, complement, district/bairro, state registration) and add default
-- consolidating-package fields used when the store quotes freight.
--
-- Existing columns already cover: address_street, address_city, address_state,
-- address_zip, address_country, cnpj. We keep those as the origin address.

ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS address_number       VARCHAR,
    ADD COLUMN IF NOT EXISTS address_complement   VARCHAR,
    ADD COLUMN IF NOT EXISTS address_district     VARCHAR,
    ADD COLUMN IF NOT EXISTS address_state_register VARCHAR,
    ADD COLUMN IF NOT EXISTS default_package_weight_grams INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS default_package_format       VARCHAR(10) NOT NULL DEFAULT 'box';

ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_package_format_check;
ALTER TABLE stores
    ADD CONSTRAINT stores_default_package_format_check
    CHECK (default_package_format IN ('box', 'roll', 'letter'));

ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_package_weight_grams_nonneg;
ALTER TABLE stores
    ADD CONSTRAINT stores_default_package_weight_grams_nonneg
    CHECK (default_package_weight_grams >= 0);

COMMENT ON COLUMN stores.address_number IS 'Street number of the shipping origin address';
COMMENT ON COLUMN stores.address_complement IS 'Optional address complement (suite, floor, etc.) of the shipping origin';
COMMENT ON COLUMN stores.address_district IS 'Neighborhood/bairro of the shipping origin address';
COMMENT ON COLUMN stores.address_state_register IS 'Inscricao estadual of the store (optional, may be ISENTO)';
COMMENT ON COLUMN stores.default_package_weight_grams IS 'Weight in grams of the consolidating package (empty box, bubble wrap) added once per shipment';
COMMENT ON COLUMN stores.default_package_format IS 'Default package format for shipping: box, roll or letter';
