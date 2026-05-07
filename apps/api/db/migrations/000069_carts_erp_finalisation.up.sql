-- Track the post-payment ERP finalisation flow on the cart itself so the
-- merchant can see, retry and unblock paid orders that failed to land in the
-- ERP. The previous flow lost the failure entirely once the webhook ACKed —
-- a paid cart with no Tiny order and no breadcrumb anywhere except the logs.
--
-- Lifecycle (only set when the cart reaches payment_status='paid'):
--   pending  → finalisation has not yet completed for a paid cart
--   done     → ERP order created, external_order_id is populated
--   failed   → CreateOrder threw; reservations were re-created so the unit
--              stays held against this cart, and the merchant has to
--              resolve via the retry button on the order page.

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS erp_finalisation_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (erp_finalisation_status IN ('pending', 'done', 'failed')),
    ADD COLUMN IF NOT EXISTS erp_last_error           TEXT,
    ADD COLUMN IF NOT EXISTS erp_last_attempt_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS erp_attempts_count       INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS erp_payment_snapshot     JSONB;

COMMENT ON COLUMN carts.erp_finalisation_status IS 'pending|done|failed — set on paid carts to track post-payment ERP order creation. failed means stock stays reserved against this cart and the merchant can retry from the admin.';
COMMENT ON COLUMN carts.erp_last_error IS 'Last error message from a failed ERP finalisation attempt. Surfaced verbatim on the order detail page.';
COMMENT ON COLUMN carts.erp_last_attempt_at IS 'Timestamp of the most recent ERP finalisation attempt (success or failure).';
COMMENT ON COLUMN carts.erp_attempts_count IS 'Total number of ERP finalisation attempts on this cart, including the initial one.';
COMMENT ON COLUMN carts.erp_payment_snapshot IS 'JSON snapshot of providers.PaymentStatus captured on first finalisation attempt. Used to replay createFinalERPOrder on retry without re-fetching from the gateway.';

-- Partial index for the admin "carts needing attention" query.
CREATE INDEX IF NOT EXISTS idx_carts_erp_finalisation_failed
    ON carts (erp_last_attempt_at DESC)
    WHERE erp_finalisation_status = 'failed';
