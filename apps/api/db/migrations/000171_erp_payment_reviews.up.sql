-- Financial observations are separate from payment_review_required, which
-- protects checkout against ambiguous receipts and must not be cleared here.
CREATE TABLE erp_payment_reviews (
    cart_id UUID PRIMARY KEY REFERENCES carts(id) ON DELETE CASCADE,
    external_order_id TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('total_below_paid', 'installments_unverified')),
    paid_cents BIGINT CHECK (paid_cents >= 0),
    order_total_cents BIGINT CHECK (order_total_cents >= 0),
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);

CREATE INDEX idx_erp_payment_reviews_open ON erp_payment_reviews(cart_id) WHERE resolved_at IS NULL;
