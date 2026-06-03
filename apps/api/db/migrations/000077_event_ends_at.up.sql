-- Optional end time for events (mainly post-commerce). NULL = no scheduled end
-- (the event runs until manually ended). Start time reuses scheduled_at.
-- There is no auto-end job: effective status is computed from the window and
-- comment processing checks the window before adding to cart.

ALTER TABLE live_events ADD COLUMN IF NOT EXISTS ends_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_live_events_ends_at
    ON live_events(ends_at) WHERE ends_at IS NOT NULL;

COMMENT ON COLUMN live_events.ends_at IS 'Optional scheduled end (UTC). NULL = manual end only.';
