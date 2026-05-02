-- cart_initial_items: frozen snapshot of the cart taken the first time the
-- buyer opens the public checkout page. Written once and immutable; the diff
-- between this and cart_items at payment time is the upsell/downsell metric.
--
-- carts.initial_snapshot_taken_at stores the moment the snapshot was frozen,
-- so the API can short-circuit the lazy snapshot path without an extra read.
-- carts.initial_subtotal_cents caches the snapshot subtotal so dashboards do
-- not have to re-aggregate cart_initial_items rows for every order listed.

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS initial_snapshot_taken_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS initial_subtotal_cents    BIGINT;

CREATE TABLE cart_initial_items (
    cart_id    UUID    NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id UUID    NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    quantity   INTEGER NOT NULL CHECK (quantity > 0),
    unit_price BIGINT  NOT NULL,
    PRIMARY KEY (cart_id, product_id)
);

COMMENT ON TABLE cart_initial_items IS
    'Immutable per-cart baseline of items present when the buyer first opened checkout.';
COMMENT ON COLUMN carts.initial_snapshot_taken_at IS
    'When the initial cart snapshot was frozen (NULL = no checkout view yet).';
COMMENT ON COLUMN carts.initial_subtotal_cents IS
    'Cached subtotal of cart_initial_items in cents.';
