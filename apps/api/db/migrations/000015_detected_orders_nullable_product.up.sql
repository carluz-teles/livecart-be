-- Make product_id nullable in detected_orders
-- This allows detecting purchase intent without knowing the specific product
-- (e.g., when a customer comments "quero 2" without specifying which product)

-- First, drop the NOT NULL constraint
ALTER TABLE detected_orders
    ALTER COLUMN product_id DROP NOT NULL;

-- Keep the existing unique constraint for orders WITH product_id
-- detected_orders_session_id_platform_user_id_product_id_key already exists

-- Create a partial unique index for orders WITHOUT product_id
-- This ensures only one "generic" order per user per session
CREATE UNIQUE INDEX detected_orders_session_user_no_product_idx
    ON detected_orders (session_id, platform_user_id)
    WHERE product_id IS NULL;
