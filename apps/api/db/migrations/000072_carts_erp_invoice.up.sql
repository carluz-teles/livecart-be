-- Track the NFe issued by the ERP (Tiny) for a paid cart. The merchant
-- always emits the NFe directly in the ERP — LiveCart never triggers
-- emission. The columns below are populated either via the Tiny webhook
-- (tipo='nota_fiscal') or via a manual "Verificar NFe" sync the merchant
-- can fire from the order detail page. Once the NFe is authorised we can
-- create the shipment at the carrier (Melhor Envio / SmartEnvios) with the
-- chave de acesso filled in.
--
-- Status mapping from Tiny situacao:
--   1 (Pendente), 4 (Aguardando Recibo), 8 (Registrada), 9 (Aguardando Protocolo) → pending
--   2 (Emitida), 6 (Autorizada), 7 (Emitida Danfe)                                → authorized
--   3 (Cancelada)                                                                 → cancelled
--   5 (Rejeitada), 10 (Denegada)                                                  → rejected

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS erp_invoice_id          TEXT,
    ADD COLUMN IF NOT EXISTS erp_invoice_key         VARCHAR(44),
    ADD COLUMN IF NOT EXISTS erp_invoice_status      TEXT
        CHECK (erp_invoice_status IS NULL OR erp_invoice_status IN ('pending', 'authorized', 'cancelled', 'rejected')),
    ADD COLUMN IF NOT EXISTS erp_invoice_emitted_at  TIMESTAMPTZ;

COMMENT ON COLUMN carts.erp_invoice_id IS 'ERP-side identifier for the issued NFe (e.g. Tiny notafiscal.id). Populated when the merchant emits the NFe in the ERP.';
COMMENT ON COLUMN carts.erp_invoice_key IS 'Chave de acesso (44 dígitos) of the issued NFe. Filled in once the NFe is authorised at SEFAZ.';
COMMENT ON COLUMN carts.erp_invoice_status IS 'pending|authorized|cancelled|rejected — normalised across ERPs. The "Criar envio" flow is unlocked when status=authorized.';
COMMENT ON COLUMN carts.erp_invoice_emitted_at IS 'Timestamp from the ERP when the NFe was emitted/authorised. Surfaced on the order detail timeline.';

-- Partial index so the admin "aguardando NFe" queries (paid orders without
-- an authorised invoice) stay snappy as the carts table grows.
CREATE INDEX IF NOT EXISTS idx_carts_awaiting_invoice
    ON carts (created_at DESC)
    WHERE payment_status = 'paid'
      AND (erp_invoice_status IS NULL OR erp_invoice_status <> 'authorized');
