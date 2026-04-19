-- Remove automatic message settings from stores
ALTER TABLE stores
DROP COLUMN IF EXISTS cart_send_on_first_item,
DROP COLUMN IF EXISTS cart_send_on_new_items,
DROP COLUMN IF EXISTS cart_message_cooldown_seconds,
DROP COLUMN IF EXISTS cart_send_expiration_reminder,
DROP COLUMN IF EXISTS cart_expiration_reminder_minutes;
