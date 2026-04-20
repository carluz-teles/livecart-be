-- Add CNPJ field to stores table
ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS cnpj VARCHAR(18);

-- Add comment for documentation
COMMENT ON COLUMN stores.cnpj IS 'Store CNPJ (Brazilian tax ID) - format: XX.XXX.XXX/XXXX-XX';
