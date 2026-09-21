CREATE FUNCTION absorb_cart_price_lots(destination uuid, source uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE item record; lot record; dest_item uuid; dest_lot uuid;
BEGIN
  IF destination=source THEN RAISE EXCEPTION 'cannot merge a cart into itself'; END IF;
  FOR item IN SELECT * FROM cart_items WHERE cart_id=source ORDER BY product_id FOR UPDATE LOOP
    FOR lot IN SELECT * FROM cart_item_price_lots WHERE cart_item_id=item.id AND quantity>0 ORDER BY sequence LOOP
      INSERT INTO cart_items(cart_id,product_id,quantity,waitlisted_quantity,unit_price,session_id)
      VALUES(destination,item.product_id,lot.quantity,lot.waitlisted_quantity,
        cart_item_request_price(destination,item.product_id,lot.unit_price,lot.session_id),lot.session_id)
      ON CONFLICT(cart_id,product_id) DO UPDATE
      SET quantity=cart_items.quantity+EXCLUDED.quantity,
          waitlisted_quantity=cart_items.waitlisted_quantity+EXCLUDED.waitlisted_quantity
      RETURNING id INTO dest_item;
      SELECT id INTO dest_lot FROM cart_item_price_lots WHERE cart_item_id=dest_item ORDER BY sequence DESC LIMIT 1;
      UPDATE cart_item_price_lots SET created_at=lot.created_at,attribution_from_log=lot.attribution_from_log WHERE id=dest_lot;
      UPDATE waitlist_items SET price_lot_id=dest_lot WHERE price_lot_id=lot.id;
    END LOOP;
    UPDATE cart_items SET paid_quantity=paid_quantity+item.paid_quantity WHERE id=dest_item;
  END LOOP;
END $$;
