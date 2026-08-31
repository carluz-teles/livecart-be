DROP INDEX IF EXISTS idx_live_events_catalog_id;
ALTER TABLE live_events DROP COLUMN IF EXISTS catalog_id;

DROP TABLE IF EXISTS catalog_products;
DROP TABLE IF EXISTS catalogs;
