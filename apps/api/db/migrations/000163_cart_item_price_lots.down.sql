DROP TRIGGER IF EXISTS cart_item_price_lots_sync ON cart_items;
DROP FUNCTION IF EXISTS sync_cart_item_price_lots();
DROP FUNCTION IF EXISTS cart_item_request_price(uuid,uuid,bigint,uuid);
DROP FUNCTION IF EXISTS cart_item_price_lots_json(uuid);
DROP FUNCTION IF EXISTS cart_item_available_total(uuid);
DROP FUNCTION IF EXISTS cart_available_total_cents(uuid);
CREATE OR REPLACE FUNCTION cart_product_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM(quantity * unit_price), 0)::bigint
  FROM cart_items WHERE cart_id = p_cart_id;
$$;
CREATE OR REPLACE FUNCTION cart_payment_fingerprint(cart uuid) RETURNS text
LANGUAGE sql STABLE AS $$
 SELECT md5(jsonb_build_object(
   'items', (SELECT jsonb_agg(jsonb_build_array(product_id,quantity,waitlisted_quantity,paid_quantity,unit_price) ORDER BY product_id) FROM cart_items WHERE cart_id=c.id),
   'shipping',c.shipping_cost_cents,'coupon',c.coupon_discount_cents,
   'pix_discount',(SELECT pix_discount_percent FROM live_events WHERE id=c.event_id)
 )::text) FROM carts c WHERE c.id=cart
$$;

DROP TABLE cart_item_price_lots;
