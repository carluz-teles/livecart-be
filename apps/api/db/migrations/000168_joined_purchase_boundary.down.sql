-- Restoring the old uniqueness rules may be impossible after a buyer started a
-- new purchase. Let the constraint reject the downgrade; never delete a cart.
DROP TRIGGER close_joined_purchase ON carts;
DROP FUNCTION close_joined_purchase();
DROP INDEX carts_one_open_per_event_buyer;
DROP INDEX carts_one_eternal_per_store_buyer;
ALTER TABLE carts DROP COLUMN purchase_closed;
CREATE UNIQUE INDEX carts_one_open_per_event_buyer ON carts(event_id,platform_user_id)
WHERE status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);
CREATE UNIQUE INDEX carts_one_eternal_per_store_buyer ON carts(store_id,platform_handle)
WHERE never_expires AND status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);
