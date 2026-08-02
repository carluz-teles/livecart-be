-- Reverte os CHECKs e as colunas do prazo estendido.
--
-- ATENÇÃO: os valores convertidos NÃO voltam. Depois do UPDATE não há como
-- distinguir uma loja que estava em 0 ("sem expiração") de uma que já estava em
-- 1440. Se for preciso desfazer de verdade, a única fonte é a lista levantada
-- antes de aplicar a migration:
--   SELECT id, name FROM stores WHERE cart_expiration_minutes = 0;
-- Guarde-a antes de rodar o up.

ALTER TABLE live_events DROP CONSTRAINT IF EXISTS live_events_cart_extended_expiration_check;
ALTER TABLE live_events DROP COLUMN IF EXISTS cart_extended_expiration_minutes;

ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_cart_extended_expiration_check;
ALTER TABLE stores DROP COLUMN IF EXISTS cart_extended_expiration_minutes;

ALTER TABLE live_events DROP CONSTRAINT IF EXISTS live_events_cart_expiration_minutes_check;
ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_cart_expiration_minutes_check;

COMMENT ON COLUMN stores.cart_expiration_minutes IS NULL;
COMMENT ON COLUMN live_events.cart_expiration_minutes IS NULL;
