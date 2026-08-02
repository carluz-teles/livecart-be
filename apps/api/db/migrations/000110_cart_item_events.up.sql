-- Log de adições ao carrinho — RN-12.
--
-- cart_items tem UNIQUE (cart_id, product_id): uma linha por produto, com a
-- quantidade somada. Ele responde "o que tem no carrinho", nunca "de onde veio
-- cada unidade". E cart_items.session_id é first-touch (o COALESCE do
-- UpsertCartItem), então 1 unidade comprada na live de segunda e 1 na de quarta
-- ficam ambas creditadas à segunda.
--
-- Numa campanha de uma semana isso é a diferença entre saber e não saber quanto
-- cada transmissão vendeu. Daí o log: uma linha por ADIÇÃO, com a sessão que a
-- gerou e o preço praticado naquele momento.
--
-- Só adições entram aqui. Remoção não é registrada como linha negativa: a
-- verdade final da quantidade continua sendo cart_items, e o selamento aloca
-- essa quantidade final sobre o log em ordem cronológica — ou seja, o que foi
-- removido sai das adições MAIS RECENTES primeiro. Isso mantém, por
-- construção, SUM(order_items.quantity) == cart_items.quantity, que é o
-- invariante que faz a soma por sessão fechar com o total do pedido (RN-29).

CREATE TABLE IF NOT EXISTS cart_item_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id     UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id  UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    -- NULL quando a adição não veio de uma transmissão (ex.: item posto pelo
    -- lojista no painel). A alocação trata isso como "sem sessão".
    session_id  UUID REFERENCES live_sessions(id) ON DELETE SET NULL,
    quantity    INTEGER NOT NULL CHECK (quantity > 0),
    unit_price  BIGINT  NOT NULL CHECK (unit_price >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A leitura do selamento: todas as adições de um carrinho, por produto, em
-- ordem cronológica. id desempata adições no mesmo instante.
CREATE INDEX IF NOT EXISTS idx_cart_item_events_cart_product
    ON cart_item_events (cart_id, product_id, created_at, id);

-- A leitura das métricas por transmissão.
CREATE INDEX IF NOT EXISTS idx_cart_item_events_session
    ON cart_item_events (session_id);

COMMENT ON TABLE cart_item_events IS
    'RN-12: uma linha por ADIÇÃO ao carrinho, com a sessão de origem. cart_items diz o que TEM no carrinho; esta tabela diz de onde veio cada unidade. Sem ela, cart_items.session_id é first-touch e credita tudo à primeira transmissão.';

COMMENT ON COLUMN cart_item_events.quantity IS
    'Quantidade ADICIONADA nesta operação (sempre > 0). Remoções não geram linha — a quantidade final vem de cart_items e o selamento aloca sobre o log, tirando das adições mais recentes primeiro.';
