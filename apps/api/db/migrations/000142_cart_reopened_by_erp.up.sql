-- O carrinho que o ERP trouxe de volta.
--
-- O lojista cancela pelo LiveCart, nós cancelamos o pedido no Tiny, e depois ele
-- reabre o pedido LÁ, pelo painel. Aconteceu em staging em 27/08/2026 e é uma
-- operação legítima: cancelou por engano, ou a compradora voltou atrás.
--
-- Antes disso o rastreamento registrava a volta e mais nada. O que ficava era um
-- pedido reservando unidade no ERP sem carrinho vivo atrás dele — peça que some
-- do disponível até alguém abrir o Tiny e reparar.
--
-- `cancellation_reverted_at` já existia para o outro caso em que um cancelamento
-- é desfeito (o pagamento que vence a corrida). Os dois são "cancelamento
-- revertido", mas por motivos diferentes, e o lojista precisa saber qual: um diz
-- "ela pagou assim mesmo", o outro diz "você reabriu no Tiny". Sem distinguir,
-- o aviso na tela teria de escolher uma das duas frases e mentir na metade das
-- vezes.

ALTER TABLE carts
    -- Por que o cancelamento foi desfeito. NULO = nunca foi.
    --   'payment_won'  — o pagamento entrou depois do cancelamento
    --   'erp_reopened' — o lojista reabriu o pedido no ERP, à mão
    ADD COLUMN IF NOT EXISTS cancellation_reverted_reason VARCHAR(32);

-- Backfill: toda reversão que já existe é do caso antigo, o do pagamento — o
-- caminho do ERP não existia até esta migration.
UPDATE carts
SET cancellation_reverted_reason = 'payment_won'
WHERE cancellation_reverted_at IS NOT NULL
  AND cancellation_reverted_reason IS NULL;

COMMENT ON COLUMN carts.cancellation_reverted_reason IS
    'Por que o cancelamento foi desfeito: payment_won (pagamento venceu a corrida) ou erp_reopened (lojista reabriu o pedido no ERP à mão)';
