CREATE TABLE live_comment_work (
    platform_comment_id text PRIMARY KEY,
    payload jsonb NOT NULL,
    lease_owner uuid,
    lease_until timestamptz,
    accepted_at timestamptz,
    accepted_plan jsonb,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX live_comment_work_pending ON live_comment_work(next_attempt_at)
    WHERE completed_at IS NULL;

ALTER TABLE cart_item_events ADD COLUMN platform_comment_id text;
ALTER TABLE cart_item_events ADD COLUMN waitlisted_quantity integer NOT NULL DEFAULT 0;
ALTER TABLE cart_item_events ADD COLUMN is_new_cart boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX cart_item_events_comment_product
    ON cart_item_events(platform_comment_id, product_id)
    WHERE platform_comment_id IS NOT NULL;

ALTER TABLE cart_items ADD COLUMN erp_confirmed_quantity integer;

ALTER TABLE carts ADD COLUMN erp_op_resting_state text;
ALTER TABLE carts ADD COLUMN erp_items_retry_at timestamptz;
ALTER TABLE carts DROP CONSTRAINT carts_erp_order_state_check;
ALTER TABLE carts ADD CONSTRAINT carts_erp_order_state_check
    CHECK (erp_order_state IN ('none','converting','open','mutating','reflecting','confirmed','cancelled'));
DROP INDEX idx_carts_erp_order_state_inflight;
CREATE INDEX idx_carts_erp_order_state_inflight ON carts(erp_order_state, erp_op_started_at)
    WHERE erp_order_state IN ('converting','mutating','reflecting');
