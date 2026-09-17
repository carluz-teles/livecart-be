-- One mutable checkpoint per product, not an append-only log. Fallback scans
-- rotate independently of metadata edits and survive process restarts.
CREATE TABLE erp_stock_sync_state (
    product_id uuid PRIMARY KEY REFERENCES products(id) ON DELETE CASCADE,
    last_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz
);
