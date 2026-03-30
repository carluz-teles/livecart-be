-- Add type field to live_events
-- 'single' = single session event (auto-ends with session)
-- 'multi' = multi-session event (manual end, supports reconnect)

ALTER TABLE live_events ADD COLUMN IF NOT EXISTS type VARCHAR NOT NULL DEFAULT 'single';

CREATE INDEX IF NOT EXISTS idx_live_events_type ON live_events(type);

COMMENT ON COLUMN live_events.type IS 'single = one live, auto-end; multi = multiple sessions, manual end';
