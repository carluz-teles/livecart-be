-- Rollback: Make product_id NOT NULL again
-- Note: This will fail if there are rows with NULL product_id

-- Drop the new unique index
DROP INDEX IF EXISTS detected_orders_session_user_product_unique;

-- Recreate the original unique constraint
ALTER TABLE detected_orders
    ADD CONSTRAINT detected_orders_session_id_platform_user_id_product_id_key
    UNIQUE (session_id, platform_user_id, product_id);

-- Add back the NOT NULL constraint
ALTER TABLE detected_orders
    ALTER COLUMN product_id SET NOT NULL;
