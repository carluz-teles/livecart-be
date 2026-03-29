-- Add auto_send_checkout_links setting to stores
ALTER TABLE stores ADD COLUMN auto_send_checkout_links BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN stores.auto_send_checkout_links IS 'When true, automatically send checkout links to customers when live ends';
