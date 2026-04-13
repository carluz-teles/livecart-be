-- Add cart settings to live_events for per-event override
-- If NULL, inherits from store settings

ALTER TABLE live_events
  ADD COLUMN IF NOT EXISTS close_cart_on_event_end BOOLEAN NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS cart_expiration_minutes INTEGER,
  ADD COLUMN IF NOT EXISTS cart_max_quantity_per_item INTEGER,
  ADD COLUMN IF NOT EXISTS auto_send_checkout_links BOOLEAN;

COMMENT ON COLUMN live_events.close_cart_on_event_end IS 'If true, cart stops accepting items when event ends';
COMMENT ON COLUMN live_events.cart_expiration_minutes IS 'Override cart expiration (NULL = use store setting)';
COMMENT ON COLUMN live_events.cart_max_quantity_per_item IS 'Override max quantity per item (NULL = use store setting)';
COMMENT ON COLUMN live_events.auto_send_checkout_links IS 'Override auto-send checkout links (NULL = use store setting)';
