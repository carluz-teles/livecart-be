-- Reverse of 000103: recreate the `payments` table exactly as it existed after
-- 000043_create_payments_table.up.sql (structure + indexes). The one-time backfill
-- INSERT from `carts` is intentionally NOT reproduced here — this restores the
-- schema so the DROP is reversible, not the historical seed data.
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
