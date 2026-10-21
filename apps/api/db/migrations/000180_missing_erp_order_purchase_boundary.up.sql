-- A missing ERP order must not re-enter the current purchase when a newer cart
-- already belongs to the buyer. Preserve the old cart, its items and ERP link.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- The IMMUTABLE function is used by both partial unique indexes. Keep readers
-- and writers out until the function and the physical index contents agree.
LOCK TABLE carts IN ACCESS EXCLUSIVE MODE;

-- Recreate the indexes rather than only REINDEX: an existing backend can have
-- the old SQL function inlined in its cached index predicate.
DROP INDEX carts_one_open_per_event_buyer;
DROP INDEX carts_one_eternal_per_store_buyer;

CREATE OR REPLACE FUNCTION erp_order_accepts_items(status TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT COALESCE(status,'') NOT IN
        ('preparando_envio','faturado','pronto_envio','enviado','entregue','nao_entregue','cancelado','nao_encontrado');
$$;

CREATE UNIQUE INDEX carts_one_open_per_event_buyer ON carts(event_id,platform_user_id)
WHERE NOT purchase_closed AND status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);

CREATE UNIQUE INDEX carts_one_eternal_per_store_buyer ON carts(store_id,platform_handle)
WHERE NOT purchase_closed AND never_expires AND status IN ('pending','active','checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'))
  AND erp_order_accepts_items(erp_order_status);
COMMIT;
