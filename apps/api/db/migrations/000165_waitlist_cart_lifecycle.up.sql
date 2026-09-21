-- Payment/cancellation/expiration closes pending demand in the SAME transaction
-- as the cart status, before an allocator or an order snapshot can observe it.
CREATE FUNCTION close_cart_waiting_requests() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE final_status text;
BEGIN
  IF NEW.status NOT IN ('cancelled','expired') AND COALESCE(NEW.payment_status,'') NOT IN ('paid','refunded') THEN
    RETURN NEW;
  END IF;
  IF OLD.status IS NOT DISTINCT FROM NEW.status AND OLD.payment_status IS NOT DISTINCT FROM NEW.payment_status THEN
    RETURN NEW;
  END IF;
  -- Lock children before their requests, matching cancellation and promotion.
  PERFORM id FROM carts WHERE joined_to_cart_id=NEW.id ORDER BY id FOR UPDATE;
  final_status := CASE WHEN NEW.status='expired' THEN 'expired' ELSE 'cancelled' END;
  UPDATE waitlist_items SET status=final_status, cancelled_at=now(),
    cancelled_quantity=cancelled_quantity+quantity,quantity=0
  WHERE cart_id IN (SELECT id FROM carts WHERE id=NEW.id OR joined_to_cart_id=NEW.id) AND status='waiting';
  UPDATE cart_items SET quantity=quantity-waitlisted_quantity,waitlisted_quantity=0
  WHERE cart_id IN (SELECT id FROM carts WHERE id=NEW.id OR joined_to_cart_id=NEW.id) AND waitlisted_quantity>0;
  DELETE FROM cart_items WHERE cart_id IN (SELECT id FROM carts WHERE id=NEW.id OR joined_to_cart_id=NEW.id) AND quantity=0;
  IF NEW.status IN ('expired','cancelled') THEN
    UPDATE waitlist_items SET status=final_status, cancelled_at=now()
    WHERE cart_id IN (SELECT id FROM carts WHERE id=NEW.id OR joined_to_cart_id=NEW.id) AND status='notified';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER close_cart_waiting_requests AFTER UPDATE OF status,payment_status ON carts
FOR EACH ROW EXECUTE FUNCTION close_cart_waiting_requests();
