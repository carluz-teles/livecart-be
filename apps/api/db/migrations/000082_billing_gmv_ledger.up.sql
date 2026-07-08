-- =============================================================================
-- LEDGER FINANCEIRO DE GMV (PRD 007 — append-only)
-- =============================================================================
-- Fonte local de verdade do GMV: alimenta o Financeiro do lojista (extrato,
-- taxa do ciclo, ROI), os numeros da plataforma (GMV/take-rate) e o fluxo de
-- estorno (credito devolvido na taxa vigente NA VENDA, mesmo em ciclo
-- posterior). Append-only: nada e editado ou apagado — estorno e uma entrada
-- nova negativa, como um extrato bancario.

CREATE TABLE billing_ledger_entries (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id     UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
  cart_id      UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
  entry_type   VARCHAR NOT NULL CHECK (entry_type IN ('sale', 'refund_credit', 'adjustment')),
  amount_cents BIGINT NOT NULL,  -- assinado: venda +, estorno −
  plan         VARCHAR NOT NULL, -- snapshot do plano na VENDA
  fee_bps      INTEGER NOT NULL, -- snapshot da taxa na VENDA
  fee_cents    BIGINT NOT NULL,  -- assinado: taxa cobrada + / creditada −
  billable     BOOLEAN NOT NULL, -- false = trial (taxa nao cobrada; estorno nao credita)
  stripe_ref   VARCHAR,          -- meter event / balance transaction (reconciliacao)
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Idempotencia: 1 venda e 1 estorno por carrinho (parcial e follow-up).
CREATE UNIQUE INDEX idx_billing_ledger_cart_type ON billing_ledger_entries (cart_id, entry_type);
CREATE INDEX idx_billing_ledger_store_time ON billing_ledger_entries (store_id, created_at DESC);
