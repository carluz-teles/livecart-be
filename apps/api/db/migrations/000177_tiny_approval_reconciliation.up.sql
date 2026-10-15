-- An approval waiting for item reconciliation is not a receipt. Keep it
-- separate from reviews of payments already recorded in erp_payment_reviews.
CREATE TABLE tiny_approval_reconciliations (
 cart_id uuid NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
 integration_id uuid NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
 external_order_id text NOT NULL,
 reason text NOT NULL DEFAULT 'approval_items_pending'
   CHECK (reason IN ('approval_items_pending','approval_payment_unverified')),
 detected_at timestamptz NOT NULL DEFAULT now(),
 checked_at timestamptz NOT NULL DEFAULT now(),
 next_check_at timestamptz NOT NULL DEFAULT now()+interval '5 minutes',
 resolved_at timestamptz,
 PRIMARY KEY (cart_id,integration_id,external_order_id)
);
CREATE INDEX tiny_approval_reconciliation_due ON tiny_approval_reconciliations(next_check_at) WHERE resolved_at IS NULL;
