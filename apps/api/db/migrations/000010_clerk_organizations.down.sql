DROP INDEX IF EXISTS idx_stores_clerk_org_id;
ALTER TABLE stores DROP COLUMN IF EXISTS clerk_org_id;
