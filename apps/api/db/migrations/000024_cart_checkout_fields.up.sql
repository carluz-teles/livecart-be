-- Add checkout-related fields to carts table
-- checkout_id: The Mercado Pago preference ID (used as external_reference)
-- checkout_expires_at: When the checkout link expires
-- customer_email: Email provided by customer for payment receipt

ALTER TABLE carts ADD COLUMN IF NOT EXISTS checkout_id VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS checkout_expires_at TIMESTAMPTZ;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS customer_email VARCHAR;

-- Index for looking up carts by checkout_id (webhook processing)
CREATE INDEX IF NOT EXISTS idx_carts_checkout_id ON carts(checkout_id) WHERE checkout_id IS NOT NULL;
