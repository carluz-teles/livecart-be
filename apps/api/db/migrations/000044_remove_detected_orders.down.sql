-- Recreate detected_orders table (for rollback)
CREATE TABLE detected_orders (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id       UUID NOT NULL REFERENCES live_sessions(id),
  cart_id          UUID REFERENCES carts(id),
  platform_user_id VARCHAR NOT NULL,
  platform_handle  VARCHAR NOT NULL,
  comment_text     TEXT,
  product_id       UUID REFERENCES products(id),  -- Nullable (allows generic orders)
  quantity         INTEGER DEFAULT 1,
  cancelled        BOOLEAN DEFAULT false,
  detected_at      TIMESTAMPTZ DEFAULT now(),
  UNIQUE (session_id, platform_user_id, product_id)
);

-- Partial unique index for orders without product_id
CREATE UNIQUE INDEX detected_orders_session_user_no_product_idx
    ON detected_orders (session_id, platform_user_id)
    WHERE product_id IS NULL;
