-- Add automatic message settings to stores
ALTER TABLE stores
ADD COLUMN IF NOT EXISTS cart_send_on_first_item BOOLEAN NOT NULL DEFAULT true,
ADD COLUMN IF NOT EXISTS cart_send_on_new_items BOOLEAN NOT NULL DEFAULT true,
ADD COLUMN IF NOT EXISTS cart_message_cooldown_seconds INTEGER NOT NULL DEFAULT 30,
ADD COLUMN IF NOT EXISTS cart_send_expiration_reminder BOOLEAN NOT NULL DEFAULT true,
ADD COLUMN IF NOT EXISTS cart_expiration_reminder_minutes INTEGER NOT NULL DEFAULT 15;

COMMENT ON COLUMN stores.cart_send_on_first_item IS 'Send automatic message when first item is added to cart';
COMMENT ON COLUMN stores.cart_send_on_new_items IS 'Send automatic message when new items are added to cart';
COMMENT ON COLUMN stores.cart_message_cooldown_seconds IS 'Minimum interval between automatic messages in seconds';
COMMENT ON COLUMN stores.cart_send_expiration_reminder IS 'Send reminder message before cart expires';
COMMENT ON COLUMN stores.cart_expiration_reminder_minutes IS 'Minutes before expiration to send reminder';
