-- Reverse the backfill by flipping rows whose attempts_count is exactly 1
-- (synthetic transition produced by the up migration) back to 'pending'.
-- Rows that experienced any real lifecycle (multiple attempts, retry runs)
-- have count > 1 and are not touched.

UPDATE carts
SET
    erp_finalisation_status = 'pending',
    erp_last_attempt_at     = NULL,
    erp_attempts_count      = 0
WHERE erp_finalisation_status = 'done'
  AND erp_attempts_count = 1
  AND external_order_id IS NOT NULL
  AND external_order_id <> '';
