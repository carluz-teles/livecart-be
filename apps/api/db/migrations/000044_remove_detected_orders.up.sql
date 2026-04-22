-- Remove detected_orders table
-- Purchase intent is now tracked directly in live_comments + carts
-- This table was used to capture raw intent from comments before creating carts

DROP TABLE IF EXISTS detected_orders;
