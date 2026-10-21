-- A buyer may have started another purchase since the upgrade. If restoring
-- the old rule creates duplicates, fail and roll back the entire migration;
-- never delete, merge, cancel or unlink carts to make the downgrade succeed.
BEGIN;
SET LOCAL lock_timeout = '5s';
LOCK TABLE carts IN ACCESS EXCLUSIVE MODE;

DROP INDEX carts_one_open_per_event_buyer;
DROP INDEX carts_one_eternal_per_store_buyer;

CREATE OR REPLACE FUNCTION erp_order_accepts_items(status TEXT) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT COALESCE(status,'') NOT IN
        ('preparando_envio','faturado','pronto_envio','enviado','entregue','nao_entregue','cancelado');
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
