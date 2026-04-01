-- Revert checkout settings from stores table
ALTER TABLE stores DROP COLUMN IF EXISTS checkout_send_methods;
ALTER TABLE stores DROP COLUMN IF EXISTS checkout_link_expiry_hours;
