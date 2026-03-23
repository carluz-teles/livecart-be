-- Revert cart_items unit_price to DECIMAL
ALTER TABLE cart_items
  ALTER COLUMN unit_price TYPE DECIMAL USING (unit_price / 100.0);

-- Add size column back to cart_items
ALTER TABLE cart_items ADD COLUMN size VARCHAR;

-- Revert products price to DECIMAL
ALTER TABLE products
  ALTER COLUMN price TYPE DECIMAL USING (price / 100.0);

-- Add sizes column back to products
ALTER TABLE products ADD COLUMN sizes TEXT[];
