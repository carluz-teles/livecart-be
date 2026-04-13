-- Remove cart settings from live_events

ALTER TABLE live_events
  DROP COLUMN IF EXISTS close_cart_on_event_end,
  DROP COLUMN IF EXISTS cart_expiration_minutes,
  DROP COLUMN IF EXISTS cart_max_quantity_per_item,
  DROP COLUMN IF EXISTS auto_send_checkout_links;
