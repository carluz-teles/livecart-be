-- Revert the provider-agnostic shipping selection changes.
-- Note: casting VARCHAR back to INTEGER will fail on rows that have non-numeric
-- ids (e.g. SmartEnvios ObjectIds). The down path here is best-effort — clear
-- such rows before reverting if needed.

ALTER TABLE carts DROP COLUMN IF EXISTS shipping_provider;

ALTER TABLE carts
    ALTER COLUMN shipping_service_id TYPE INTEGER USING NULLIF(shipping_service_id, '')::integer;
