CREATE OR REPLACE FUNCTION cart_unpaid_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  WITH lots AS (
    SELECT ci.paid_quantity,l.quantity,l.waitlisted_quantity,l.unit_price,
      COALESCE(SUM(l.quantity-l.waitlisted_quantity) OVER
        (PARTITION BY ci.id ORDER BY l.created_at,l.sequence ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS before_qty
    FROM cart_items ci JOIN cart_item_price_lots l ON l.cart_item_id=ci.id
    WHERE ci.cart_id=p_cart_id
  )
  SELECT COALESCE(SUM((quantity-waitlisted_quantity-LEAST(quantity-waitlisted_quantity,
    GREATEST(0,paid_quantity-before_qty)))*unit_price),0)::bigint FROM lots;
$$;
