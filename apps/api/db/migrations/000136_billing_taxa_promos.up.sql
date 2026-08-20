-- Taxa (comissão GMV) promos: desconto sobre a comissão, aplicado quando o
-- reactor de ciclo monta o InvoiceItem. Separado do cupom de mensalidade (que
-- é nativo na Stripe). discount_bps = 5000 → 50%. cycles_remaining = quantas
-- faturas de ciclo o desconto ainda cobre (decrementa a cada aplicação).
CREATE TABLE billing_taxa_promos (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id          UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
  discount_bps      INT  NOT NULL CHECK (discount_bps > 0 AND discount_bps <= 10000),
  cycles_remaining  INT  NOT NULL CHECK (cycles_remaining >= 0),
  code              TEXT,        -- promo de origem (ex.: CANTODAART), informativo
  description       TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Busca do reactor: promo ativa da loja (cycles_remaining > 0).
CREATE INDEX idx_billing_taxa_promos_active
  ON billing_taxa_promos (store_id)
  WHERE cycles_remaining > 0;
