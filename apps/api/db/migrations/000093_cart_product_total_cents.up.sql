CREATE OR REPLACE FUNCTION cart_product_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM(quantity * unit_price), 0)::bigint
  FROM cart_items WHERE cart_id = p_cart_id;
$$;
