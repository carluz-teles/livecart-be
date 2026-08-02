DROP INDEX IF EXISTS notifications_recipient_cart_type_uniq;

DELETE FROM notifications WHERE type = 'order_cancellation_reverted';

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_type_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_type_check
    CHECK (type IN (
        'idea_comment',
        'idea_reply',
        'idea_status_change'
    ));

ALTER TABLE notifications DROP COLUMN IF EXISTS cart_id;

ALTER TABLE carts DROP COLUMN IF EXISTS cancellation_reverted_at;
