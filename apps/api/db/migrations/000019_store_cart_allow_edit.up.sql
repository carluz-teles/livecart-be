-- Add cart_allow_edit setting to stores table
ALTER TABLE stores
  ADD COLUMN cart_allow_edit BOOLEAN NOT NULL DEFAULT true;

COMMENT ON COLUMN stores.cart_allow_edit IS 'Allow customers to edit cart (remove items, change quantity) on checkout page';
