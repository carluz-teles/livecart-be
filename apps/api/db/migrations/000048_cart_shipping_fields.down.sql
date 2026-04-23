ALTER TABLE carts
    DROP COLUMN IF EXISTS shipping_service_id,
    DROP COLUMN IF EXISTS shipping_service_name,
    DROP COLUMN IF EXISTS shipping_carrier,
    DROP COLUMN IF EXISTS shipping_cost_cents,
    DROP COLUMN IF EXISTS shipping_cost_real_cents,
    DROP COLUMN IF EXISTS shipping_deadline_days,
    DROP COLUMN IF EXISTS shipping_quoted_at;
