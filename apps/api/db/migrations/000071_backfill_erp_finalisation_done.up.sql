-- Counterpart to 000070's "failed" backfill: mark pending+has-external-order
-- carts as 'done'. These are paid carts that successfully landed in the ERP
-- before erp_finalisation_status existed, so their lifecycle column stuck at
-- the column default 'pending' instead of advancing to 'done'.
--
-- Without this, every previously-completed paid cart shows up under
-- "Para despachar" (paid + no shipment yet) AND still answers 'pending' to
-- any future audit / dashboard that filters by erp_finalisation_status — a
-- silent inconsistency between lifecycle column and reality.
--
-- Idempotent: skips rows already at 'done' or 'failed'. erp_attempts_count
-- is bumped by 1 to indicate the synthetic transition; erp_last_attempt_at
-- mirrors paid_at for traceability.

UPDATE carts
SET
    erp_finalisation_status = 'done',
    erp_last_attempt_at     = COALESCE(erp_last_attempt_at, paid_at, created_at),
    erp_attempts_count      = erp_attempts_count + 1
WHERE erp_finalisation_status = 'pending'
  AND external_order_id IS NOT NULL
  AND external_order_id <> '';
