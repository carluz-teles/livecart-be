DROP TABLE IF EXISTS cart_initial_items;

ALTER TABLE carts
    DROP COLUMN IF EXISTS initial_subtotal_cents,
    DROP COLUMN IF EXISTS initial_snapshot_taken_at;
