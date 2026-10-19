-- Retain invoices received before the local order is materialised. The binding
-- and observation time also prevent a delayed read from replacing newer data.
CREATE TABLE cart_invoice_observations (
    cart_id UUID PRIMARY KEY REFERENCES carts(id) ON DELETE CASCADE,
    store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    integration_id UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    external_order_id TEXT NOT NULL CHECK (external_order_id <> ''),
    invoice_id TEXT NOT NULL CHECK (invoice_id <> ''),
    invoice_key TEXT NOT NULL DEFAULT '',
    invoice_status TEXT NOT NULL DEFAULT '',
    emitted_at TIMESTAMPTZ,
    observed_at TIMESTAMPTZ NOT NULL,
    applied_at TIMESTAMPTZ
);
CREATE INDEX idx_cart_invoice_observations_pending
    ON cart_invoice_observations(observed_at, cart_id) WHERE applied_at IS NULL;
CREATE INDEX idx_cart_invoice_observations_store ON cart_invoice_observations(store_id);
CREATE INDEX idx_cart_invoice_observations_integration ON cart_invoice_observations(integration_id);
