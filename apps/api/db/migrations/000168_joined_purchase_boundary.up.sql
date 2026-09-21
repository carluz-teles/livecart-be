-- A joined child may keep its historical financial state while its host closes
-- the purchase. This boundary releases the buyer's next cart without inventing
-- a child payment, cancelling a real order, or removing the original links.
ALTER TABLE carts ADD COLUMN purchase_closed boolean NOT NULL DEFAULT false;
UPDATE carts child SET purchase_closed=true
FROM carts host WHERE child.joined_to_cart_id=host.id
  AND (host.status IN ('cancelled','expired') OR host.payment_status IN ('paid','refunded'));

DROP INDEX carts_one_open_per_event_buyer;
CREATE UNIQUE INDEX carts_one_open_per_event_buyer ON carts(event_id,platform_user_id)
WHERE NOT purchase_closed AND status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);
DROP INDEX carts_one_eternal_per_store_buyer;
CREATE UNIQUE INDEX carts_one_eternal_per_store_buyer ON carts(store_id,platform_handle)
WHERE NOT purchase_closed AND never_expires AND status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);

CREATE FUNCTION close_joined_purchase() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status IN ('cancelled','expired') OR NEW.payment_status IN ('paid','refunded') THEN
    UPDATE carts SET purchase_closed=true
    WHERE joined_to_cart_id=NEW.id AND NOT purchase_closed;
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER close_joined_purchase AFTER UPDATE OF status,payment_status ON carts
FOR EACH ROW EXECUTE FUNCTION close_joined_purchase();
