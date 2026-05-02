-- cart_mutations: append-only audit of buyer-driven cart edits at checkout.
-- One row per atomic change (add/remove/quantity bump). The merchant
-- dashboard reads this to compute upsell/downsell metrics and reconstruct
-- the timeline between the initial cart snapshot and the paid order.

CREATE TABLE cart_mutations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id         UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    mutation_type   VARCHAR NOT NULL CHECK (mutation_type IN
                        ('item_added','item_removed','quantity_increased','quantity_decreased')),
    quantity_before INTEGER NOT NULL,
    quantity_after  INTEGER NOT NULL,
    unit_price      BIGINT  NOT NULL,
    source          VARCHAR NOT NULL DEFAULT 'buyer_checkout'
                        CHECK (source IN ('buyer_checkout','live_add','merchant')),
    erp_movement_id VARCHAR,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cart_mutations_cart_created ON cart_mutations (cart_id, created_at);
CREATE INDEX idx_cart_mutations_product ON cart_mutations (product_id);

COMMENT ON TABLE cart_mutations IS
    'Append-only log of cart item mutations during checkout (buyer or merchant driven).';
