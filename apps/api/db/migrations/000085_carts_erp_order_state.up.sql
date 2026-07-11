-- Design C ("pedido na iniciação do pagamento"): o pedido de venda do Tiny
-- passa a ser criado quando o comprador inicia o pagamento e vira a própria
-- reserva de estoque (lançado, situação Aberta). O ciclo de vida é rastreado
-- por cart:
--   none       — modelo legado (saídas manuais); default de todo cart
--   converting — conversão em voo (CAS none→converting é o single-flight);
--                NUNCA é resetado para none — o sweep adota via marcador
--   open       — pedido Aberta + estoque lançado; edições via ciclo
--                estornar → PUT /itens → lançar
--   mutating   — ciclo de mutação em voo (CAS open→mutating)
--   confirmed  — pagamento confirmado (parcelas gravadas + situação Aprovada)
--   cancelled  — cart expirado/cancelado (pedido cancelado + estoque estornado)
ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS erp_order_state      VARCHAR(16) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS erp_stock_launched   BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS erp_op_started_at    TIMESTAMPTZ;

ALTER TABLE carts
    ADD CONSTRAINT carts_erp_order_state_check
    CHECK (erp_order_state IN ('none','converting','open','mutating','confirmed','cancelled'));

-- Sweep de operações presas (converting/mutating velhos) roda periodicamente.
CREATE INDEX IF NOT EXISTS idx_carts_erp_order_state_inflight
    ON carts (erp_order_state, erp_op_started_at)
    WHERE erp_order_state IN ('converting','mutating');
