-- Remove CNPJ field from stores table
ALTER TABLE stores
    DROP COLUMN IF EXISTS cnpj;
