ALTER TABLE carts
    DROP COLUMN IF EXISTS last_shipping_quote_at,
    DROP COLUMN IF EXISTS last_shipping_quote_options;
