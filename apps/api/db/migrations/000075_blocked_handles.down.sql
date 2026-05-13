ALTER TABLE carts DROP COLUMN IF EXISTS cancelled_reason;
DROP INDEX IF EXISTS idx_blocked_handles_active;
DROP TABLE IF EXISTS blocked_handles;
