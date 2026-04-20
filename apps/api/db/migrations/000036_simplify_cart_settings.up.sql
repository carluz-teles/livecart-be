-- Simplify cart settings:
-- 1. Consolidate cart_send_on_first_item + cart_send_on_new_items → cart_real_time
-- 2. Rename auto_send_checkout_links → send_on_live_end (on both stores and live_events)
-- 3. Remove checkout_link_expiry_hours (calculated from cart_expiration_minutes)
-- 4. Remove cart_notify_before_expiration (redundant with cart_send_expiration_reminder)

-- Step 1: Add new consolidated column
ALTER TABLE stores ADD COLUMN IF NOT EXISTS cart_real_time BOOLEAN NOT NULL DEFAULT true;

-- Step 2: Migrate data - cart_real_time = true if BOTH send_on_first_item AND send_on_new_items were true
UPDATE stores SET cart_real_time = (cart_send_on_first_item AND cart_send_on_new_items);

-- Step 3: Rename auto_send_checkout_links to send_on_live_end (stores table)
ALTER TABLE stores RENAME COLUMN auto_send_checkout_links TO send_on_live_end;

-- Step 4: Rename auto_send_checkout_links to send_on_live_end (live_events table)
ALTER TABLE live_events RENAME COLUMN auto_send_checkout_links TO send_on_live_end;

-- Step 5: Drop deprecated columns
ALTER TABLE stores DROP COLUMN IF EXISTS cart_send_on_first_item;
ALTER TABLE stores DROP COLUMN IF EXISTS cart_send_on_new_items;
ALTER TABLE stores DROP COLUMN IF EXISTS checkout_link_expiry_hours;
ALTER TABLE stores DROP COLUMN IF EXISTS cart_notify_before_expiration;

-- Add comment for clarity
COMMENT ON COLUMN stores.cart_real_time IS 'When true, sends message every time customer adds item. When false, only sends at end of live.';
COMMENT ON COLUMN stores.send_on_live_end IS 'When true, automatically sends checkout links when live ends.';
COMMENT ON COLUMN live_events.send_on_live_end IS 'Override send_on_live_end (NULL = use store setting)';
