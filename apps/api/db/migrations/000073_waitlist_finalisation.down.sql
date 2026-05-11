DROP INDEX IF EXISTS idx_waitlist_cart_active;

ALTER TABLE waitlist_items
    DROP CONSTRAINT IF EXISTS waitlist_items_status_check;

ALTER TABLE waitlist_items
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS notification_sent_at,
    DROP COLUMN IF EXISTS cart_id;

ALTER TABLE live_events
    DROP COLUMN IF EXISTS waitlist_notified_ttl_minutes;
