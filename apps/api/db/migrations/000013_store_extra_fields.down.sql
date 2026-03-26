-- Remove extra fields from stores table
ALTER TABLE stores
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS website,
    DROP COLUMN IF EXISTS logo_url,
    DROP COLUMN IF EXISTS address_street,
    DROP COLUMN IF EXISTS address_city,
    DROP COLUMN IF EXISTS address_state,
    DROP COLUMN IF EXISTS address_zip,
    DROP COLUMN IF EXISTS address_country;
