-- Restore platform_live_id as NOT NULL (update any nulls first)
UPDATE live_sessions ls
SET platform_live_id = COALESCE(
    (SELECT platform_live_id FROM live_session_platforms WHERE session_id = ls.id LIMIT 1),
    ''
)
WHERE platform_live_id IS NULL;

ALTER TABLE live_sessions ALTER COLUMN platform_live_id SET NOT NULL;

-- Drop the new table
DROP TABLE live_session_platforms;
