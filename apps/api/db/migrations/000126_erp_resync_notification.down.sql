DELETE FROM notifications WHERE type = 'erp_resync_finished';

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_type_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_type_check
    CHECK (type IN (
        'idea_comment',
        'idea_reply',
        'idea_status_change',
        'order_cancellation_reverted'
    ));
