-- Keep retry scheduling separate from the financial observation timestamp.
-- Only existing, unresolved observations are checked; this creates no orders.
ALTER TABLE erp_payment_reviews
    ADD COLUMN next_check_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX idx_erp_payment_reviews_next_check
    ON erp_payment_reviews(next_check_at, cart_id)
    WHERE resolved_at IS NULL AND reason = 'installments_unverified';
