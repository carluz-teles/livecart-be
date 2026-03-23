-- stores
CREATE TABLE stores (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name            VARCHAR NOT NULL,
  slug            VARCHAR UNIQUE NOT NULL,
  active          BOOLEAN DEFAULT true,
  whatsapp_number VARCHAR,
  email_address   VARCHAR,
  sms_number      VARCHAR,
  created_at      TIMESTAMPTZ DEFAULT now()
);

-- store_users
CREATE TABLE store_users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id      UUID NOT NULL REFERENCES stores(id),
  email         VARCHAR NOT NULL,
  role          VARCHAR NOT NULL,
  password_hash TEXT,
  created_at    TIMESTAMPTZ DEFAULT now()
);

-- products
CREATE TABLE products (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id        UUID NOT NULL REFERENCES stores(id),
  name            VARCHAR NOT NULL,
  external_id     VARCHAR,
  external_source VARCHAR NOT NULL,
  keyword         CHAR(4) NOT NULL,
  price           DECIMAL,
  image_url       TEXT,
  sizes           TEXT[],
  stock           INTEGER DEFAULT 0,
  active          BOOLEAN DEFAULT true,
  created_at      TIMESTAMPTZ DEFAULT now(),
  updated_at      TIMESTAMPTZ DEFAULT now(),
  UNIQUE (store_id, keyword),
  UNIQUE (store_id, external_source, external_id)
);

-- integrations (must come before carts due to FK)
CREATE TABLE integrations (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id         UUID NOT NULL REFERENCES stores(id),
  type             VARCHAR NOT NULL,
  provider         VARCHAR NOT NULL,
  status           VARCHAR NOT NULL DEFAULT 'pending_auth',
  access_token     TEXT,
  refresh_token    TEXT,
  token_expires_at TIMESTAMPTZ,
  extra_config     JSONB,
  last_synced_at   TIMESTAMPTZ,
  created_at       TIMESTAMPTZ DEFAULT now()
);

-- live_sessions
CREATE TABLE live_sessions (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id         UUID NOT NULL REFERENCES stores(id),
  platform         VARCHAR NOT NULL,
  platform_live_id VARCHAR NOT NULL,
  status           VARCHAR NOT NULL DEFAULT 'active',
  started_at       TIMESTAMPTZ DEFAULT now(),
  ended_at         TIMESTAMPTZ,
  total_comments   INTEGER DEFAULT 0,
  total_orders     INTEGER DEFAULT 0
);

-- carts
CREATE TABLE carts (
  id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id             UUID NOT NULL REFERENCES live_sessions(id),
  platform_user_id       VARCHAR NOT NULL,
  platform_handle        VARCHAR NOT NULL,
  token                  VARCHAR UNIQUE NOT NULL,
  status                 VARCHAR NOT NULL DEFAULT 'pending',
  checkout_url           TEXT,
  payment_integration_id UUID REFERENCES integrations(id),
  external_order_id      VARCHAR,
  payment_status         VARCHAR DEFAULT 'unpaid',
  paid_at                TIMESTAMPTZ,
  notify_status          VARCHAR DEFAULT 'pending',
  notify_error           TEXT,
  notified_at            TIMESTAMPTZ,
  created_at             TIMESTAMPTZ DEFAULT now(),
  expires_at             TIMESTAMPTZ,
  UNIQUE (session_id, platform_user_id)
);

-- detected_orders
CREATE TABLE detected_orders (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id       UUID NOT NULL REFERENCES live_sessions(id),
  cart_id          UUID REFERENCES carts(id),
  platform_user_id VARCHAR NOT NULL,
  platform_handle  VARCHAR NOT NULL,
  comment_text     TEXT,
  product_id       UUID NOT NULL REFERENCES products(id),
  quantity         INTEGER DEFAULT 1,
  cancelled        BOOLEAN DEFAULT false,
  detected_at      TIMESTAMPTZ DEFAULT now(),
  UNIQUE (session_id, platform_user_id, product_id)
);

-- cart_items
CREATE TABLE cart_items (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  cart_id    UUID NOT NULL REFERENCES carts(id),
  product_id UUID NOT NULL REFERENCES products(id),
  size       VARCHAR,
  quantity   INTEGER DEFAULT 1,
  unit_price DECIMAL
);

-- integration_logs
CREATE TABLE integration_logs (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  integration_id   UUID NOT NULL REFERENCES integrations(id),
  entity_type      VARCHAR,
  entity_id        UUID,
  direction        VARCHAR,
  status           VARCHAR,
  request_payload  JSONB,
  response_payload JSONB,
  error_message    TEXT,
  created_at       TIMESTAMPTZ DEFAULT now()
);

-- subscriptions
CREATE TABLE subscriptions (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id                 UUID NOT NULL REFERENCES stores(id),
  integration_id           UUID NOT NULL REFERENCES integrations(id),
  external_subscription_id VARCHAR,
  status                   VARCHAR NOT NULL DEFAULT 'trialing',
  current_period_start     TIMESTAMPTZ,
  current_period_end       TIMESTAMPTZ,
  cancelled_at             TIMESTAMPTZ,
  created_at               TIMESTAMPTZ DEFAULT now()
);
