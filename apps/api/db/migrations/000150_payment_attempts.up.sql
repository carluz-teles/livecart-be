CREATE FUNCTION cart_payment_fingerprint(cart uuid) RETURNS text
LANGUAGE sql STABLE AS $$
 SELECT md5(jsonb_build_object(
   'items', (SELECT jsonb_agg(jsonb_build_array(product_id,quantity,waitlisted_quantity,paid_quantity,unit_price) ORDER BY product_id) FROM cart_items WHERE cart_id=c.id),
   'shipping',c.shipping_cost_cents,'coupon',c.coupon_discount_cents,
   'pix_discount',(SELECT pix_discount_percent FROM live_events WHERE id=c.event_id)
 )::text) FROM carts c WHERE c.id=cart
$$;
CREATE TABLE payment_attempts (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 cart_id uuid NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
 integration_id uuid NOT NULL REFERENCES integrations(id),
 provider text NOT NULL,
 amount_cents bigint NOT NULL CHECK(amount_cents>=0),
 cart_fingerprint text NOT NULL,
 payment_id text,
 cancel_id text,
 status text NOT NULL DEFAULT 'created' CHECK(status IN ('created','pending','paid','cancelled','review_required')),
 review_reason text,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_attempts_cart ON payment_attempts(cart_id,created_at);
CREATE UNIQUE INDEX payment_attempts_provider_payment ON payment_attempts(integration_id,payment_id) WHERE payment_id IS NOT NULL;
ALTER TABLE carts ADD COLUMN payment_review_required boolean NOT NULL DEFAULT false;
ALTER TABLE carts ADD COLUMN pix_cancel_lease_until timestamptz;
