-- Create payments table (1:N relationship with carts)
-- Supports multiple payment attempts, refunds, and partial payments
CREATE TABLE payments (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  cart_id              UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
  integration_id       UUID REFERENCES integrations(id) ON DELETE SET NULL,

  -- Payment provider reference
  external_payment_id  VARCHAR,         -- Payment ID from provider (e.g., Mercado Pago payment ID)
  provider             VARCHAR NOT NULL, -- mercado_pago, pagarme, etc.

  -- Amount and method
  amount_cents         BIGINT NOT NULL,
  currency             VARCHAR DEFAULT 'BRL',
  method               VARCHAR,          -- pix, credit_card, debit_card, boleto

  -- Status tracking
  status               VARCHAR NOT NULL DEFAULT 'pending', -- pending, processing, approved, rejected, cancelled, refunded
  status_detail        VARCHAR,          -- Provider-specific detail

  -- Provider response (for debugging and auditing)
  provider_response    JSONB,

  -- Timestamps
  created_at           TIMESTAMPTZ DEFAULT now(),
  updated_at           TIMESTAMPTZ DEFAULT now(),
  paid_at              TIMESTAMPTZ,

  -- For idempotency (prevent duplicate payments)
  idempotency_key      VARCHAR UNIQUE
);

-- Indexes for common queries
CREATE INDEX idx_payments_cart_id ON payments(cart_id);
CREATE INDEX idx_payments_external_id ON payments(external_payment_id) WHERE external_payment_id IS NOT NULL;
CREATE INDEX idx_payments_status ON payments(status);
CREATE INDEX idx_payments_created_at ON payments(created_at DESC);

-- Migrate existing payment data from carts to payments table
-- Only migrate carts that have payment activity (not unpaid/new carts)
INSERT INTO payments (
    cart_id,
    integration_id,
    external_payment_id,
    provider,
    amount_cents,
    method,
    status,
    paid_at,
    created_at
)
SELECT
    c.id as cart_id,
    c.payment_integration_id as integration_id,
    COALESCE(c.checkout_id, c.external_order_id) as external_payment_id,
    COALESCE(i.provider, 'unknown') as provider,
    COALESCE((
        SELECT SUM(ci.quantity * ci.unit_price)::BIGINT
        FROM cart_items ci
        WHERE ci.cart_id = c.id
    ), 0) as amount_cents,
    c.payment_method as method,
    CASE
        WHEN c.payment_status = 'paid' THEN 'approved'
        WHEN c.payment_status = 'failed' THEN 'rejected'
        WHEN c.payment_status = 'cancelled' THEN 'cancelled'
        WHEN c.payment_status = 'refunded' THEN 'refunded'
        ELSE 'pending'
    END as status,
    c.paid_at,
    COALESCE(c.paid_at, c.created_at) as created_at
FROM carts c
LEFT JOIN integrations i ON i.id = c.payment_integration_id
WHERE c.payment_status IS NOT NULL
  AND c.payment_status NOT IN ('unpaid', '');
