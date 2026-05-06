-- Public order-tracking page reads a cart by `short_id` + `tracking_token`.
-- Token is generated once when the cart transitions to paid (postcheckout
-- package) and is never rotated — the customer can revisit the same URL from
-- their email forever. NULL means the cart has not been paid yet.

ALTER TABLE carts
  ADD COLUMN tracking_token TEXT;

-- Lookup index for the public endpoint. Partial: only paid carts have tokens,
-- so we skip the NULL bloat.
CREATE UNIQUE INDEX IF NOT EXISTS idx_carts_tracking_token
  ON carts (tracking_token)
  WHERE tracking_token IS NOT NULL;
