-- Persist the acquirer authorization code (NSU) returned by the card gateway
-- so the public checkout receipt can display it for the customer / suporte.
-- Only populated for card payments processed via the transparent flow; PIX
-- and pre-existing paid carts leave this NULL and the public response omits
-- the field.

ALTER TABLE carts ADD COLUMN IF NOT EXISTS card_authorization_code VARCHAR;
