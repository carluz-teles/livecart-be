-- Reverte a Fatia B1. shipping_service_id volta a INT; ids opacos não-numéricos
-- (que só existiam na fonte carts) viram NULL — a leitura pré-B1 lia de carts
-- de qualquer forma, então nada de valor se perde no rollback.
ALTER TABLE order_logistics
    ALTER COLUMN shipping_service_id TYPE INT
    USING (CASE WHEN shipping_service_id ~ '^[0-9]+$' THEN shipping_service_id::int ELSE NULL END);

ALTER TABLE order_logistics
    DROP COLUMN IF EXISTS shipping_provider;

ALTER TABLE orders
    DROP COLUMN IF EXISTS customer_snapshot;
