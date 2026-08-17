DROP INDEX IF EXISTS idx_products_barcode;
ALTER TABLE products DROP COLUMN IF EXISTS barcode;
