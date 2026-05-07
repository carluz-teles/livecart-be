DROP INDEX IF EXISTS idx_carts_erp_finalisation_failed;

ALTER TABLE carts
    DROP COLUMN IF EXISTS erp_payment_snapshot,
    DROP COLUMN IF EXISTS erp_attempts_count,
    DROP COLUMN IF EXISTS erp_last_attempt_at,
    DROP COLUMN IF EXISTS erp_last_error,
    DROP COLUMN IF EXISTS erp_finalisation_status;
