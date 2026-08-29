DROP INDEX IF EXISTS idx_cart_items_unpaid;
DROP FUNCTION IF EXISTS cart_unpaid_total_cents(uuid);
DROP TRIGGER IF EXISTS trg_cart_items_clamp_paid_quantity ON cart_items;
DROP FUNCTION IF EXISTS cart_items_clamp_paid_quantity();
ALTER TABLE carts DROP COLUMN IF EXISTS paid_amount_cents;
ALTER TABLE cart_items DROP COLUMN IF EXISTS paid_quantity;
