-- Revert cart settings simplification

-- Step 1: Restore original columns
ALTER TABLE stores ADD COLUMN IF NOT EXISTS cart_send_on_first_item BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS cart_send_on_new_items BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS checkout_link_expiry_hours INTEGER DEFAULT 48;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS cart_notify_before_expiration BOOLEAN NOT NULL DEFAULT true;

-- Step 2: Migrate data back
UPDATE stores SET
  cart_send_on_first_item = cart_real_time,
  cart_send_on_new_items = cart_real_time,
  checkout_link_expiry_hours = GREATEST(1, CEIL(cart_expiration_minutes::float / 60)),
  cart_notify_before_expiration = cart_send_expiration_reminder;

-- Step 3: Rename send_on_live_end back to auto_send_checkout_links
ALTER TABLE stores RENAME COLUMN send_on_live_end TO auto_send_checkout_links;

-- Step 4: Drop the new consolidated column
ALTER TABLE stores DROP COLUMN IF EXISTS cart_real_time;
