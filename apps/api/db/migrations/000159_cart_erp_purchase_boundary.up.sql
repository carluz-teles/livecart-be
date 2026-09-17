-- Keep the lookup and both uniqueness guards on the same ERP boundary.
-- Existing orders, items and payments are preserved.
CREATE FUNCTION erp_order_accepts_items(status TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT COALESCE(status,'') NOT IN
        ('preparando_envio','faturado','pronto_envio','enviado','entregue','nao_entregue','cancelado');
$$;

DROP INDEX carts_one_open_per_event_buyer;
CREATE UNIQUE INDEX carts_one_open_per_event_buyer ON carts (event_id,platform_user_id)
    WHERE status IN ('pending','active','checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
      AND erp_order_accepts_items(erp_order_status);

DROP INDEX carts_one_eternal_per_store_buyer;
CREATE UNIQUE INDEX carts_one_eternal_per_store_buyer ON carts (store_id,platform_handle)
    WHERE never_expires AND status IN ('pending','active','checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
      AND erp_order_accepts_items(erp_order_status);

ALTER TABLE live_comment_work ADD COLUMN erp_blocked_at TIMESTAMPTZ;
