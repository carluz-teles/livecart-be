-- Add title and timestamps to live_sessions
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS title VARCHAR DEFAULT '';
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT now();
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT now();
