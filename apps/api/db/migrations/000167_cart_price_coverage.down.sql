CREATE OR REPLACE FUNCTION cart_unpaid_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM((quantity-paid_quantity)*unit_price),0)::bigint
  FROM cart_items WHERE cart_id=p_cart_id AND paid_quantity<quantity;
$$;
