-- Add extra fields to stores table for organization settings
ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS description TEXT,
    ADD COLUMN IF NOT EXISTS website VARCHAR(255),
    ADD COLUMN IF NOT EXISTS logo_url VARCHAR(500),
    ADD COLUMN IF NOT EXISTS address_street VARCHAR(255),
    ADD COLUMN IF NOT EXISTS address_city VARCHAR(100),
    ADD COLUMN IF NOT EXISTS address_state VARCHAR(50),
    ADD COLUMN IF NOT EXISTS address_zip VARCHAR(20),
    ADD COLUMN IF NOT EXISTS address_country VARCHAR(100) DEFAULT 'Brasil';

-- Add comments for documentation
COMMENT ON COLUMN stores.description IS 'Store description/bio for public display';
COMMENT ON COLUMN stores.website IS 'Store external website URL';
COMMENT ON COLUMN stores.logo_url IS 'Store logo image URL';
COMMENT ON COLUMN stores.address_street IS 'Store street address';
COMMENT ON COLUMN stores.address_city IS 'Store city';
COMMENT ON COLUMN stores.address_state IS 'Store state/province';
COMMENT ON COLUMN stores.address_zip IS 'Store postal/ZIP code';
COMMENT ON COLUMN stores.address_country IS 'Store country';
