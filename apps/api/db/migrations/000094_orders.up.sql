CREATE TABLE orders (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  cart_id          UUID NOT NULL UNIQUE REFERENCES carts(id),
  short_id         INT  NOT NULL,
  store_id         UUID NOT NULL,
  event_id         UUID NOT NULL,
  customer_id      UUID,
  status           TEXT NOT NULL DEFAULT 'paid',
  -- Totais congelados no cart.paid (imutáveis).
  total_cents      BIGINT,
  discount_cents   BIGINT NOT NULL DEFAULT 0,
  shipping_cents   BIGINT NOT NULL DEFAULT 0,
  paid_total_cents BIGINT,
  paid_at          TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_orders_store    ON orders(store_id);
CREATE INDEX idx_orders_event    ON orders(event_id);
CREATE INDEX idx_orders_customer ON orders(customer_id);

CREATE TABLE order_items (
  id           UUID   PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id     UUID   NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  product_id   UUID   NOT NULL,
  product_name TEXT   NOT NULL,
  quantity     INT    NOT NULL,
  unit_price   BIGINT NOT NULL
);
CREATE INDEX idx_order_items_order ON order_items(order_id);

CREATE TABLE order_payments (
  order_id              UUID PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
  payment_status        TEXT   NOT NULL DEFAULT 'pending',
  payment_method        TEXT,
  card_snapshot         JSONB,
  gateway_snapshot      JSONB,
  coupon_id             UUID,
  coupon_code           TEXT,
  coupon_discount_cents BIGINT NOT NULL DEFAULT 0,
  external_order_id     TEXT,
  erp_finalisation_status TEXT NOT NULL DEFAULT 'pending',
  erp_last_error        TEXT,
  erp_last_attempt_at   TIMESTAMPTZ,
  erp_attempts_count    INT  NOT NULL DEFAULT 0,
  invoice_id            TEXT,
  invoice_key           VARCHAR(44),
  invoice_status        TEXT,
  invoice_emitted_at    TIMESTAMPTZ
);

CREATE TABLE order_logistics (
  order_id                 UUID PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
  shipping_address         JSONB,
  shipping_service_id      INT,
  shipping_service_name    TEXT,
  shipping_carrier         TEXT,
  shipping_cost_cents      BIGINT,
  shipping_cost_real_cents BIGINT,
  shipping_deadline_days   INT,
  tracking_token           TEXT,
  shipment_status          TEXT,
  shipment_provider        TEXT,
  provider_order_id        TEXT,
  tracking_code            TEXT,
  public_tracking_url      TEXT,
  delivered_at             TIMESTAMPTZ,
  erp_order_state          TEXT    NOT NULL DEFAULT 'none',
  erp_stock_launched       BOOLEAN NOT NULL DEFAULT false,
  erp_op_started_at        TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_order_logistics_tracking
  ON order_logistics(tracking_token) WHERE tracking_token IS NOT NULL;
