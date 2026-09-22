-- One bounded checkpoint per product. Revisions coalesce overlapping webhook
-- reads without dropping a later invalidation (including A -> B -> A).
ALTER TABLE erp_stock_sync_state
  ADD COLUMN requested_revision bigint NOT NULL DEFAULT 0,
  ADD COLUMN completed_revision bigint NOT NULL DEFAULT 0,
  ADD COLUMN read_owner uuid,
  ADD COLUMN read_until timestamptz;
