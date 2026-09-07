-- Stop workers before rolling back. A reflection only reads the ERP; restore
-- its resting state without scheduling an outbound grid replacement.
UPDATE carts SET erp_order_state = CASE
    WHEN erp_op_resting_state IN ('open','confirmed') THEN erp_op_resting_state
    WHEN payment_status='paid' THEN 'confirmed' ELSE 'open' END
WHERE erp_order_state='reflecting';
ALTER TABLE carts DROP CONSTRAINT carts_erp_order_state_check;
ALTER TABLE carts ADD CONSTRAINT carts_erp_order_state_check
    CHECK (erp_order_state IN ('none','converting','open','mutating','confirmed','cancelled'));
DROP INDEX idx_carts_erp_order_state_inflight;
CREATE INDEX idx_carts_erp_order_state_inflight ON carts(erp_order_state, erp_op_started_at)
    WHERE erp_order_state IN ('converting','mutating');
ALTER TABLE carts DROP COLUMN erp_op_resting_state;
ALTER TABLE carts DROP COLUMN erp_items_retry_at;
ALTER TABLE cart_items DROP COLUMN erp_confirmed_quantity;
DROP INDEX cart_item_events_comment_product;
ALTER TABLE cart_item_events DROP COLUMN is_new_cart;
ALTER TABLE cart_item_events DROP COLUMN waitlisted_quantity;
ALTER TABLE cart_item_events DROP COLUMN platform_comment_id;
DROP TABLE live_comment_work;
