-- Revert event scheduling columns
DROP INDEX IF EXISTS idx_live_events_scheduled_at;
ALTER TABLE live_events DROP COLUMN IF EXISTS scheduled_at;
ALTER TABLE live_events DROP COLUMN IF EXISTS description;

-- Drop event upsells
DROP TABLE IF EXISTS event_upsells;

-- Drop event products
DROP TABLE IF EXISTS event_products;
