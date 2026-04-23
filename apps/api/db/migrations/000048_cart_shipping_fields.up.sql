-- Persist the freight option chosen by the customer on the cart.
-- shipping_cost_cents is what we charge the customer (zero if the event is
-- marked free_shipping); shipping_cost_real_cents is the actual quote value
-- so the merchant always knows how much the shipment will cost.

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS shipping_service_id      INTEGER,
    ADD COLUMN IF NOT EXISTS shipping_service_name    VARCHAR,
    ADD COLUMN IF NOT EXISTS shipping_carrier         VARCHAR,
    ADD COLUMN IF NOT EXISTS shipping_cost_cents      BIGINT,
    ADD COLUMN IF NOT EXISTS shipping_cost_real_cents BIGINT,
    ADD COLUMN IF NOT EXISTS shipping_deadline_days   INTEGER,
    ADD COLUMN IF NOT EXISTS shipping_quoted_at       TIMESTAMPTZ;

COMMENT ON COLUMN carts.shipping_service_id IS 'ID of the shipping service at the provider (Melhor Envio service ID)';
COMMENT ON COLUMN carts.shipping_service_name IS 'Human name of the service, e.g. PAC, SEDEX, Jadlog .Package';
COMMENT ON COLUMN carts.shipping_carrier IS 'Carrier behind the service: Correios, Jadlog, Loggi, etc.';
COMMENT ON COLUMN carts.shipping_cost_cents IS 'What we charge the customer for shipping (zero for free-shipping events)';
COMMENT ON COLUMN carts.shipping_cost_real_cents IS 'Actual shipping quote value - what the shipment will cost the merchant';
COMMENT ON COLUMN carts.shipping_deadline_days IS 'Estimated maximum delivery time in business days';
COMMENT ON COLUMN carts.shipping_quoted_at IS 'When the quote was last refreshed. Quotes expire and must be recomputed after ~1h';
