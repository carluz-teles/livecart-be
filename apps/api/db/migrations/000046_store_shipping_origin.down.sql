ALTER TABLE stores
    DROP CONSTRAINT IF EXISTS stores_default_package_format_check,
    DROP CONSTRAINT IF EXISTS stores_default_package_weight_grams_nonneg;

ALTER TABLE stores
    DROP COLUMN IF EXISTS address_number,
    DROP COLUMN IF EXISTS address_complement,
    DROP COLUMN IF EXISTS address_district,
    DROP COLUMN IF EXISTS address_state_register,
    DROP COLUMN IF EXISTS default_package_weight_grams,
    DROP COLUMN IF EXISTS default_package_format;
