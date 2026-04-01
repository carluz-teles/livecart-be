-- Add checkout settings to stores table
-- checkout_link_expiry_hours: How long the checkout page is valid (default 48h)
-- checkout_send_methods: JSON array of enabled notification methods

ALTER TABLE stores ADD COLUMN IF NOT EXISTS checkout_link_expiry_hours INT DEFAULT 48;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS checkout_send_methods JSONB DEFAULT '["public_link", "manual"]';

-- Note: checkout_send_methods options:
-- "public_link" - Customer accesses /cart/{token} directly
-- "manual" - Staff manually sends the link to customer
-- "instagram_dm" - (Future) Auto-send via Instagram Direct Message
