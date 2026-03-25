-- Add cart settings to stores table
ALTER TABLE stores
  ADD COLUMN cart_enabled BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN cart_expiration_minutes INTEGER NOT NULL DEFAULT 30,
  ADD COLUMN cart_reserve_stock BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN cart_max_items INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN cart_max_quantity_per_item INTEGER NOT NULL DEFAULT 5,
  ADD COLUMN cart_notify_before_expiration BOOLEAN NOT NULL DEFAULT true;

-- Add updated_at column to stores (was missing)
ALTER TABLE stores
  ADD COLUMN updated_at TIMESTAMPTZ DEFAULT now();

-- Add comment explaining the defaults
COMMENT ON COLUMN stores.cart_enabled IS 'Whether the cart system is enabled for this store';
COMMENT ON COLUMN stores.cart_expiration_minutes IS 'Minutes before cart expires (0 = no expiration)';
COMMENT ON COLUMN stores.cart_reserve_stock IS 'Reserve stock when item is added to cart';
COMMENT ON COLUMN stores.cart_max_items IS 'Max different items per cart (0 = unlimited)';
COMMENT ON COLUMN stores.cart_max_quantity_per_item IS 'Max quantity of same item (0 = unlimited)';
COMMENT ON COLUMN stores.cart_notify_before_expiration IS 'Notify customer before cart expires';
