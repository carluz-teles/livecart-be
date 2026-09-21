-- Keep independent requests on rollback; restoring the former buyer/product
-- unique index would delete valid requests created by the new behavior.
DROP TRIGGER IF EXISTS cancel_deleted_waitlist_lot ON cart_item_price_lots;
DROP TRIGGER IF EXISTS cancel_reduced_waitlist_lot ON cart_item_price_lots;
DROP FUNCTION IF EXISTS cancel_reduced_waitlist_lot();
DROP TRIGGER IF EXISTS prepare_waitlist_request ON waitlist_items;
DROP FUNCTION IF EXISTS prepare_waitlist_request();
DROP INDEX IF EXISTS idx_waitlist_global_fifo;
DROP INDEX IF EXISTS uq_waitlist_source_command;
ALTER TABLE waitlist_items DROP COLUMN IF EXISTS price_lot_id,
    DROP COLUMN IF EXISTS queue_sequence, DROP COLUMN IF EXISTS source_command,
    DROP COLUMN IF EXISTS cancelled_quantity,DROP COLUMN IF EXISTS fulfilled_quantity,
    DROP COLUMN IF EXISTS original_quantity,DROP COLUMN IF EXISTS unit_price;
