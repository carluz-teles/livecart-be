-- Reverte a Fatia 10-b: re-adiciona as 9 colunas pós-venda do ERP no cart, com as
-- CHECKs/defaults/comments idênticos às migrations 000069 (finalização) e 000072
-- (NFe), e recria os 3 índices parciais (069/072/084) que dependiam delas. O
-- valor histórico NÃO é restaurado — vive em order_payments desde a Fatia 11b.

-- Finalização (idêntico à 000069).
ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS erp_finalisation_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (erp_finalisation_status IN ('pending', 'done', 'failed')),
    ADD COLUMN IF NOT EXISTS erp_last_error           TEXT,
    ADD COLUMN IF NOT EXISTS erp_last_attempt_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS erp_attempts_count       INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS erp_payment_snapshot     JSONB;

COMMENT ON COLUMN carts.erp_finalisation_status IS 'pending|done|failed — set on paid carts to track post-payment ERP order creation. failed means stock stays reserved against this cart and the merchant can retry from the admin.';
COMMENT ON COLUMN carts.erp_last_error IS 'Last error message from a failed ERP finalisation attempt. Surfaced verbatim on the order detail page.';
COMMENT ON COLUMN carts.erp_last_attempt_at IS 'Timestamp of the most recent ERP finalisation attempt (success or failure).';
COMMENT ON COLUMN carts.erp_attempts_count IS 'Total number of ERP finalisation attempts on this cart, including the initial one.';
COMMENT ON COLUMN carts.erp_payment_snapshot IS 'JSON snapshot of providers.PaymentStatus captured on first finalisation attempt. Used to replay createFinalERPOrder on retry without re-fetching from the gateway.';

-- NFe (idêntico à 000072).
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

-- Índices parciais dependentes (069/072/084).
CREATE INDEX IF NOT EXISTS idx_carts_erp_finalisation_failed
    ON carts (erp_last_attempt_at DESC)
    WHERE erp_finalisation_status = 'failed';

CREATE INDEX IF NOT EXISTS idx_carts_awaiting_invoice
    ON carts (created_at DESC)
    WHERE payment_status = 'paid'
      AND (erp_invoice_status IS NULL OR erp_invoice_status <> 'authorized');

CREATE INDEX IF NOT EXISTS idx_carts_paid_erp_in_flight
    ON carts (payment_status, erp_finalisation_status)
    WHERE payment_status = 'paid' AND erp_finalisation_status <> 'done';
