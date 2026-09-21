DROP TRIGGER IF EXISTS freeze_waitlist_deadline_eligibility ON live_events;
DROP FUNCTION IF EXISTS freeze_waitlist_deadline_eligibility();
DROP TRIGGER IF EXISTS capture_commercial_close ON live_events;
DROP FUNCTION IF EXISTS capture_commercial_close();
ALTER TABLE carts
    DROP COLUMN deadline_config_y_minutes,
    DROP COLUMN deadline_config_x_minutes,
    DROP COLUMN deadline_config_base_at,
    DROP COLUMN waitlist_extra_eligible;
ALTER TABLE live_events DROP COLUMN commercial_closed_at;
-- Keep the expanded CHECK: shrinking it would reject legitimate persisted Y
-- values. A rollback must not silently replace merchants' selected settings.
