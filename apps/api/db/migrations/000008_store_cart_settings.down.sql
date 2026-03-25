-- Remove cart settings from stores table
ALTER TABLE stores
  DROP COLUMN IF EXISTS cart_enabled,
  DROP COLUMN IF EXISTS cart_expiration_minutes,
  DROP COLUMN IF EXISTS cart_reserve_stock,
  DROP COLUMN IF EXISTS cart_max_items,
  DROP COLUMN IF EXISTS cart_max_quantity_per_item,
  DROP COLUMN IF EXISTS cart_notify_before_expiration,
  DROP COLUMN IF EXISTS updated_at;
