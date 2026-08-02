-- Atribuição por sessão no pedido selado — RN-13 e RN-29.
--
-- Depois do cutover, receita CONFIRMADA sai de orders. Sem sessão em
-- order_items, "quanto a live de terça faturou" é impossível de responder pela
-- fonte da verdade — só pelo carrinho, que é mutável.
--
-- POR QUE NÃO BASTA UMA COLUNA COPIADA DE cart_items.
--
-- order_items é materializado 1:1 a partir de cart_items, que tem
-- UNIQUE (cart_id, product_id). Copiar cart_items.session_id para cá
-- reproduziria exatamente o first-touch que a RN-12 rejeita: 2 unidades
-- compradas em transmissões diferentes viriam com a sessão da primeira.
--
-- Então a mudança é de CARDINALIDADE, não uma coluna a mais: o pedido passa a
-- ter uma linha por (produto, sessão). Um pedido com o mesmo vestido comprado
-- na segunda e na quarta tem duas linhas de vestido, cada uma com sua sessão e
-- o preço praticado naquele momento. A UI agrupa na exibição.
--
-- Não há constraint a dropar: order_items nunca teve unique em
-- (order_id, product_id) — verificado em \d order_items. A cardinalidade era
-- consequência do materializador, não do schema.

ALTER TABLE order_items
    ADD COLUMN IF NOT EXISTS session_id UUID REFERENCES live_sessions(id) ON DELETE SET NULL;

-- A leitura de "quanto esta transmissão faturou".
CREATE INDEX IF NOT EXISTS idx_order_items_session
    ON order_items (session_id);

COMMENT ON COLUMN order_items.session_id IS
    'RN-13: transmissão que originou ESTAS unidades. NULL = adição sem sessão (item posto pelo painel) ou pedido anterior ao log de atribuição. Um pedido pode ter várias linhas do mesmo produto, uma por sessão.';

COMMENT ON TABLE order_items IS
    'Itens congelados do pedido. Uma linha por (produto, sessão) desde a 000109 — a UI agrupa por produto na exibição. SUM(quantity) por produto continua igual à quantidade do carrinho no pagamento.';
