ALTER TABLE erp_stock_sync_state
  DROP COLUMN requested_revision,
  DROP COLUMN completed_revision,
  DROP COLUMN read_owner,
  DROP COLUMN read_until;
