-- Keep unresolved edits and retained stock until reconciliation. No order data
-- is changed by this migration.
ALTER TABLE cart_erp_edits ADD COLUMN blocked_at timestamptz;
-- A deferred stock notification is transferred to this bounded checkpoint,
-- rather than discarded or retried indefinitely in Redis.
ALTER TABLE erp_stock_sync_state ADD COLUMN deferred_at timestamptz;
