-- Create customers table (per-store scope)
-- Customers are identified by platform_user_id (Instagram user ID) per store
CREATE TABLE customers (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id          UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
  platform_user_id  VARCHAR NOT NULL,
  platform_handle   VARCHAR NOT NULL,
  email             VARCHAR,
  phone             VARCHAR,
  first_order_at    TIMESTAMPTZ,
  last_order_at     TIMESTAMPTZ,
  created_at        TIMESTAMPTZ DEFAULT now(),
  updated_at        TIMESTAMPTZ DEFAULT now(),
  UNIQUE (store_id, platform_user_id)
);

-- Indexes for common queries
CREATE INDEX idx_customers_store_id ON customers(store_id);
CREATE INDEX idx_customers_platform_handle ON customers(store_id, platform_handle);
CREATE INDEX idx_customers_last_order_at ON customers(store_id, last_order_at DESC);

-- Add customer_id FK to carts table (nullable for backwards compatibility)
ALTER TABLE carts ADD COLUMN customer_id UUID REFERENCES customers(id) ON DELETE SET NULL;
CREATE INDEX idx_carts_customer_id ON carts(customer_id) WHERE customer_id IS NOT NULL;

-- Migrate existing customer data from carts
-- Use a CTE to get aggregated data per store+platform_user
WITH customer_data AS (
    SELECT DISTINCT ON (le.store_id, c.platform_user_id)
        le.store_id,
        c.platform_user_id,
        c.platform_handle,
        c.customer_email as email,
        MIN(c.created_at) OVER w as first_order_at,
        MAX(c.created_at) OVER w as last_order_at
    FROM carts c
    JOIN live_events le ON le.id = c.event_id
    WHERE c.platform_user_id IS NOT NULL
      AND c.platform_user_id != ''
    WINDOW w AS (PARTITION BY le.store_id, c.platform_user_id)
    ORDER BY le.store_id, c.platform_user_id, c.created_at DESC
)
INSERT INTO customers (store_id, platform_user_id, platform_handle, email, first_order_at, last_order_at)
SELECT store_id, platform_user_id, platform_handle, email, first_order_at, last_order_at
FROM customer_data;

-- Link existing carts to their customers
UPDATE carts c
SET customer_id = cust.id
FROM customers cust
JOIN live_events le ON le.store_id = cust.store_id
WHERE c.event_id = le.id
  AND c.platform_user_id = cust.platform_user_id;
