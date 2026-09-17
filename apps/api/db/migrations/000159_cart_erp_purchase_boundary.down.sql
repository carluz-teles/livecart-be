-- The former constraints can only be restored if no buyer has opened a new
-- purchase alongside an invoiced, locally unpaid order. PostgreSQL aborts this
-- migration on a conflict; never delete or merge orders to make rollback fit.
DROP INDEX carts_one_open_per_event_buyer;
CREATE UNIQUE INDEX carts_one_open_per_event_buyer ON carts (event_id,platform_user_id)
    WHERE status IN ('pending','active','checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'));
DROP INDEX carts_one_eternal_per_store_buyer;
CREATE UNIQUE INDEX carts_one_eternal_per_store_buyer ON carts (store_id,platform_handle)
    WHERE never_expires AND status IN ('pending','active','checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid','refunded'));
ALTER TABLE live_comment_work DROP COLUMN erp_blocked_at;
DROP FUNCTION erp_order_accepts_items(TEXT);
