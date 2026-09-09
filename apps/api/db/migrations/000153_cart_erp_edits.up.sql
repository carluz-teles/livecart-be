-- Durable merchant edits. The item, stock reservation and retry record commit together.
CREATE TABLE cart_erp_edits (
    cart_id uuid PRIMARY KEY REFERENCES carts(id) ON DELETE CASCADE,
    revision bigint NOT NULL DEFAULT 0,
    synced_revision bigint NOT NULL DEFAULT 0,
    queued_at timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts integer NOT NULL DEFAULT 0,
    lease_owner uuid,
    lease_until timestamptz,
    last_error text,
    CHECK (revision >= synced_revision)
);
CREATE INDEX cart_erp_edits_pending ON cart_erp_edits(next_attempt_at)
    WHERE revision > synced_revision;
CREATE TABLE cart_erp_edit_requests (
    id uuid PRIMARY KEY,
    cart_id uuid NOT NULL REFERENCES cart_erp_edits(cart_id) ON DELETE CASCADE,
    revision bigint NOT NULL,
    request jsonb NOT NULL,
    product_id uuid NOT NULL REFERENCES products(id),
    retained_quantity integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cart_erp_edit_requests_cart ON cart_erp_edit_requests(cart_id, revision);
CREATE INDEX cart_erp_edit_requests_product ON cart_erp_edit_requests(product_id, cart_id);
