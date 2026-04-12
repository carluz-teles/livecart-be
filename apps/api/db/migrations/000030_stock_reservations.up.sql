CREATE TABLE stock_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    cart_id UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    external_product_id VARCHAR NOT NULL,
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    erp_movement_id VARCHAR,
    status VARCHAR NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'reversed', 'converted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reversed_at TIMESTAMPTZ
);

CREATE INDEX idx_stock_reservations_event_status ON stock_reservations(event_id, status);
CREATE INDEX idx_stock_reservations_cart_product_status ON stock_reservations(cart_id, product_id, status);
CREATE INDEX idx_stock_reservations_ext_product_status ON stock_reservations(external_product_id, status);

-- Prevent duplicate active reservations for the same cart+product+event
CREATE UNIQUE INDEX uq_stock_reservations_active
    ON stock_reservations(cart_id, product_id, event_id)
    WHERE status = 'active';
