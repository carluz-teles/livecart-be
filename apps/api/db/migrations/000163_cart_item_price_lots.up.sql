-- Keep each addition's agreed price. The cart_items row remains the public
-- identity; lots carry quantities/prices and survive partial fulfillment.
CREATE TABLE cart_item_price_lots (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  sequence bigserial NOT NULL UNIQUE,
  cart_item_id uuid NOT NULL REFERENCES cart_items(id) ON DELETE CASCADE,
  quantity integer NOT NULL CHECK (quantity >= 0),
  waitlisted_quantity integer NOT NULL CHECK (waitlisted_quantity >= 0 AND waitlisted_quantity <= quantity),
  unit_price bigint NOT NULL CHECK (unit_price >= 0),
  session_id uuid REFERENCES live_sessions(id) ON DELETE SET NULL,
  attribution_from_log boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX cart_item_price_lots_item ON cart_item_price_lots(cart_item_id, sequence);

-- Legacy carts have one authoritative price. Do not invent prices from the
-- current catalogue or reconstruct removed quantities from addition-only logs.
INSERT INTO cart_item_price_lots(cart_item_id,quantity,waitlisted_quantity,unit_price,session_id,attribution_from_log)
SELECT id,quantity,waitlisted_quantity,unit_price,session_id,true FROM cart_items;

-- INSERT ... ON CONFLICT keeps the existing cart line's display price. Pass
-- the incoming addition price to its AFTER trigger, including the conflict case.
CREATE FUNCTION cart_item_request_price(cart uuid, product uuid, price bigint, session uuid)
RETURNS bigint LANGUAGE plpgsql VOLATILE AS $$
BEGIN
  PERFORM set_config('livecart.item_addition', jsonb_build_object(
    'cart',cart,'product',product,'price',price,'session',session)::text,true);
  RETURN price;
END $$;

CREATE FUNCTION sync_cart_item_price_lots() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  addition jsonb;
  targeted uuid;
  old_qty integer := 0;
  old_wait integer := 0;
  qty_delta integer;
  wait_delta integer;
  amount integer;
  taken integer;
  row_lot record;
  price bigint;
  origin_session uuid;
BEGIN
  IF current_setting('livecart.price_lots_override',true) = NEW.id::text THEN
    RETURN NEW;
  END IF;
  addition := NULLIF(current_setting('livecart.item_addition',true),'')::jsonb;
  targeted := NULLIF(current_setting('livecart.waitlist_price_lot_id',true),'')::uuid;
  PERFORM set_config('livecart.item_addition','',true);
  PERFORM set_config('livecart.waitlist_price_lot_id','',true);
  IF TG_OP = 'UPDATE' THEN
    old_qty := OLD.quantity;
    old_wait := OLD.waitlisted_quantity;
    IF NEW.unit_price IS DISTINCT FROM OLD.unit_price THEN
      -- An explicit line-price edit is a merchant/ERP correction, never a
      -- catalogue refresh. Apply that explicit correction to its surviving lots.
      UPDATE cart_item_price_lots SET unit_price=NEW.unit_price WHERE cart_item_id=NEW.id;
    END IF;
  END IF;
  qty_delta := NEW.quantity-old_qty;
  wait_delta := NEW.waitlisted_quantity-old_wait;
  IF qty_delta > 0 THEN
    price := NEW.unit_price;
    origin_session := NEW.session_id;
    IF addition->>'cart'=NEW.cart_id::text AND addition->>'product'=NEW.product_id::text THEN
      price := (addition->>'price')::bigint;
      origin_session := (addition->>'session')::uuid;
    END IF;
    taken := LEAST(qty_delta,GREATEST(0,wait_delta));
    INSERT INTO cart_item_price_lots(cart_item_id,quantity,waitlisted_quantity,unit_price,session_id,attribution_from_log)
    VALUES(NEW.id,qty_delta,taken,price,origin_session,
      addition->>'cart' IS DISTINCT FROM NEW.cart_id::text OR addition->>'product' IS DISTINCT FROM NEW.product_id::text);
    wait_delta := wait_delta-taken;
  ELSIF qty_delta < 0 THEN
    -- Reductions remove pending requests first, newest first; allocated units
    -- are then removed newest first. A leave-queue action can target one lot.
    amount := LEAST(-qty_delta,GREATEST(0,-wait_delta));
    FOR row_lot IN SELECT * FROM cart_item_price_lots
      WHERE cart_item_id=NEW.id AND waitlisted_quantity>0
      ORDER BY (id=targeted) DESC NULLS LAST, created_at DESC,sequence DESC FOR UPDATE
    LOOP
      EXIT WHEN amount=0;
      taken := LEAST(amount,row_lot.waitlisted_quantity);
      UPDATE cart_item_price_lots SET quantity=quantity-taken,waitlisted_quantity=waitlisted_quantity-taken WHERE id=row_lot.id;
      amount := amount-taken;
      qty_delta := qty_delta+taken;
      wait_delta := wait_delta+taken;
    END LOOP;
    amount := -qty_delta;
    FOR row_lot IN SELECT * FROM cart_item_price_lots
      WHERE cart_item_id=NEW.id AND quantity>waitlisted_quantity
      ORDER BY created_at DESC,sequence DESC FOR UPDATE
    LOOP
      EXIT WHEN amount=0;
      taken := LEAST(amount,row_lot.quantity-row_lot.waitlisted_quantity);
      UPDATE cart_item_price_lots SET quantity=quantity-taken WHERE id=row_lot.id;
      amount := amount-taken;
    END LOOP;
    IF amount <> 0 THEN RAISE EXCEPTION 'cart price lots cannot satisfy reduction'; END IF;
  END IF;
  IF wait_delta < 0 THEN
    amount := -wait_delta;
    FOR row_lot IN SELECT * FROM cart_item_price_lots
      WHERE cart_item_id=NEW.id AND waitlisted_quantity>0
      ORDER BY (id=targeted) DESC NULLS LAST, created_at,sequence FOR UPDATE
    LOOP
      EXIT WHEN amount=0;
      taken := LEAST(amount,row_lot.waitlisted_quantity);
      UPDATE cart_item_price_lots SET waitlisted_quantity=waitlisted_quantity-taken WHERE id=row_lot.id;
      amount := amount-taken;
    END LOOP;
    IF amount <> 0 THEN RAISE EXCEPTION 'cart price lots cannot satisfy promotion'; END IF;
  ELSIF wait_delta > 0 THEN
    amount := wait_delta;
    FOR row_lot IN SELECT * FROM cart_item_price_lots
      WHERE cart_item_id=NEW.id AND quantity>waitlisted_quantity
      ORDER BY created_at DESC,sequence DESC FOR UPDATE
    LOOP
      EXIT WHEN amount=0;
      taken := LEAST(amount,row_lot.quantity-row_lot.waitlisted_quantity);
      UPDATE cart_item_price_lots SET waitlisted_quantity=waitlisted_quantity+taken WHERE id=row_lot.id;
      amount := amount-taken;
    END LOOP;
    IF amount <> 0 THEN RAISE EXCEPTION 'cart price lots cannot satisfy waiting split'; END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER cart_item_price_lots_sync AFTER INSERT OR UPDATE OF quantity,waitlisted_quantity,unit_price
ON cart_items FOR EACH ROW EXECUTE FUNCTION sync_cart_item_price_lots();

CREATE FUNCTION cart_item_price_lots_json(item uuid) RETURNS jsonb LANGUAGE sql STABLE AS $$
  SELECT COALESCE(jsonb_agg(jsonb_build_object('quantity',quantity,
    'waitlistedQuantity',waitlisted_quantity,'unitPrice',unit_price,
    'totalPrice',(quantity-waitlisted_quantity)*unit_price) ORDER BY created_at,sequence),'[]'::jsonb)
  FROM cart_item_price_lots WHERE cart_item_id=item AND quantity>0;
$$;
CREATE FUNCTION cart_item_available_total(item uuid) RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM((quantity-waitlisted_quantity)*unit_price),0)::bigint
  FROM cart_item_price_lots WHERE cart_item_id=item;
$$;
CREATE OR REPLACE FUNCTION cart_product_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM(l.quantity*l.unit_price),0)::bigint
  FROM cart_items ci JOIN cart_item_price_lots l ON l.cart_item_id=ci.id WHERE ci.cart_id=p_cart_id;
$$;
CREATE FUNCTION cart_available_total_cents(cart uuid) RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM((l.quantity-l.waitlisted_quantity)*l.unit_price),0)::bigint
  FROM cart_items ci JOIN cart_item_price_lots l ON l.cart_item_id=ci.id WHERE ci.cart_id=cart;
$$;

-- Keep existing single-price fingerprints compatible with pending gateway
-- attempts; only mixed-price carts need the additional canonical price vector.
CREATE OR REPLACE FUNCTION cart_payment_fingerprint(cart uuid) RETURNS text
LANGUAGE sql STABLE AS $$
 SELECT md5((jsonb_build_object(
   'items',(SELECT jsonb_agg(jsonb_build_array(product_id,quantity,waitlisted_quantity,paid_quantity,unit_price) ORDER BY product_id) FROM cart_items WHERE cart_id=c.id),
   'shipping',c.shipping_cost_cents,'coupon',c.coupon_discount_cents,
   'pix_discount',(SELECT pix_discount_percent FROM live_events WHERE id=c.event_id)
 ) || CASE WHEN EXISTS (SELECT 1 FROM cart_items ci JOIN cart_item_price_lots l ON l.cart_item_id=ci.id
       WHERE ci.cart_id=c.id AND l.quantity>0 AND l.unit_price<>ci.unit_price)
 THEN jsonb_build_object('price_lots',(SELECT jsonb_agg(jsonb_build_array(ci.product_id,l.unit_price,l.quantity,l.waitlisted_quantity) ORDER BY ci.product_id,l.created_at,l.sequence)
      FROM cart_items ci JOIN cart_item_price_lots l ON l.cart_item_id=ci.id WHERE ci.cart_id=c.id AND l.quantity>0))
 ELSE '{}'::jsonb END)::text) FROM carts c WHERE c.id=cart;
$$;
