-- Revert: Remove session_id from carts
DROP INDEX IF EXISTS idx_carts_session_id;
ALTER TABLE carts DROP COLUMN IF EXISTS session_id;
