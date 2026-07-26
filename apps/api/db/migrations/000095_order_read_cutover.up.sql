-- Fatia B1 — cutover da LEITURA do detalhe do pedido para as tabelas order_*.
--
-- 1. Congela o customer (name/email/document/phone) num JSONB imutável na Order,
--    para o detalhe deixar de depender de carts.customer_*.
-- 2. order_logistics ganha shipping_provider e passa shipping_service_id para TEXT,
--    espelhando a semântica de carts (id opaco: int-as-string p/ ME, ObjectId/UUID
--    p/ outros provedores) — antes era INT e perdia ids não-numéricos.
-- 3. Backfill das orders já materializadas a partir do cart de origem.

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS customer_snapshot JSONB;

ALTER TABLE order_logistics
    ADD COLUMN IF NOT EXISTS shipping_provider TEXT;

-- INT -> TEXT preserva ids opacos. Valores INT existentes convertem sem perda.
ALTER TABLE order_logistics
    ALTER COLUMN shipping_service_id TYPE TEXT USING shipping_service_id::text;

-- Backfill customer_snapshot das orders pagas a partir de carts.customer_*.
UPDATE orders o
SET customer_snapshot = jsonb_build_object(
        'name',     COALESCE(c.customer_name, ''),
        'email',    COALESCE(c.customer_email, ''),
        'document', COALESCE(c.customer_document, ''),
        'phone',    COALESCE(c.customer_phone, '')
    )
FROM carts c
WHERE c.id = o.cart_id
  AND o.customer_snapshot IS NULL;

-- Backfill shipping_provider + shipping_service_id (opaco) a partir do cart.
UPDATE order_logistics ol
SET shipping_provider   = c.shipping_provider,
    shipping_service_id = c.shipping_service_id
FROM orders o
JOIN carts c ON c.id = o.cart_id
WHERE ol.order_id = o.id;
