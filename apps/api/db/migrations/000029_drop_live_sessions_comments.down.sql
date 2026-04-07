ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS comments JSONB DEFAULT '[]'::jsonb;
