DROP INDEX IF EXISTS idx_carts_tracking_token;

ALTER TABLE carts
  DROP COLUMN IF EXISTS tracking_token;
