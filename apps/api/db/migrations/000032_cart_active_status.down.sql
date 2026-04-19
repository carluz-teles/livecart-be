-- Rollback: active -> pending
UPDATE carts SET status = 'pending' WHERE status = 'active';

COMMENT ON COLUMN carts.status IS NULL;
