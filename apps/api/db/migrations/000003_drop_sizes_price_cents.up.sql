-- Drop sizes column from products
ALTER TABLE products DROP COLUMN sizes;

-- Convert price from DECIMAL to BIGINT (cents)
-- First, convert existing values to cents (multiply by 100)
ALTER TABLE products
  ALTER COLUMN price TYPE BIGINT USING (COALESCE(price, 0) * 100)::BIGINT;

-- Drop size column from cart_items
ALTER TABLE cart_items DROP COLUMN size;

-- Convert unit_price from DECIMAL to BIGINT (cents)
ALTER TABLE cart_items
  ALTER COLUMN unit_price TYPE BIGINT USING (COALESCE(unit_price, 0) * 100)::BIGINT;
