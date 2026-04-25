-- Cache the latest shipping quote response per cart so SelectShippingMethod
-- can resolve the customer's selection without re-quoting. Re-quoting fails
-- for providers (e.g. SmartEnvios) whose `id` is a per-quotation token that
-- changes on every /quote/freight call — the id the customer just clicked
-- never appears in a fresh response, so the option looks "indisponível".

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS last_shipping_quote_options JSONB,
    ADD COLUMN IF NOT EXISTS last_shipping_quote_at      TIMESTAMPTZ;

COMMENT ON COLUMN carts.last_shipping_quote_options IS 'Snapshot of the options array from the most recent QuoteShipping response — exact shape the public API returned to the customer.';
COMMENT ON COLUMN carts.last_shipping_quote_at IS 'When the snapshot above was generated. Selections older than the cache TTL should force a re-quote on the client.';
