-- Make the shipping selection on carts provider-agnostic.
-- Melhor Envio uses integer ids; SmartEnvios returns MongoDB ObjectIds; future
-- providers will use UUIDs. Store the service id as an opaque string and add
-- a shipping_provider column so we know which integration owns the selection.

ALTER TABLE carts
    ALTER COLUMN shipping_service_id TYPE VARCHAR USING shipping_service_id::text,
    ADD COLUMN IF NOT EXISTS shipping_provider VARCHAR;

COMMENT ON COLUMN carts.shipping_service_id IS 'Opaque service id returned by the shipping provider (int-as-string for ME, ObjectId/UUID for others)';
COMMENT ON COLUMN carts.shipping_provider IS 'Name of the shipping integration that owns this selection: melhor_envio | smartenvios | ...';
