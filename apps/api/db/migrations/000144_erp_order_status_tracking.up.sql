-- Rastreamento da situação do pedido no ERP, dentro do LiveCart.
--
-- O pedido de venda passou a ser criado no primeiro comentário da live e a viver
-- meses no ERP: nasce Em aberto, é aprovado no pagamento, o lojista fatura,
-- separa, despacha, entrega. Todo esse trajeto acontecia fora do LiveCart e não
-- voltava — quem quisesse saber onde o pedido estava abria o ERP.
--
-- O ERP avisa: o webhook de vendas dispara em cada transição com o
-- `codigoSituacao` novo. Só faltava guardar.
--
-- Duas colunas de estado no carrinho (onde o pedido está AGORA, que é o que a
-- tela pergunta) e uma tabela de histórico (por onde passou e quando, que é o
-- que a dúvida do lojista pergunta).

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS erp_order_status VARCHAR(32),
    ADD COLUMN IF NOT EXISTS erp_order_status_at TIMESTAMPTZ,
    -- Número humano do pedido no ERP ("34"), que é como o lojista o chama ao
    -- telefone. O external_order_id é o id interno (367938409) e não serve para
    -- essa conversa.
    ADD COLUMN IF NOT EXISTS erp_order_number VARCHAR(32);

-- Índice para a varredura de reconciliação: pedidos parados num estágio não
-- terminal são exatamente os que podem ter perdido um webhook. Parcial pelo
-- external_order_id porque a imensa maioria dos carrinhos nunca chega a ter
-- pedido; a situação NULA fica DENTRO do índice de propósito — é o carrinho que
-- perdeu o primeiro aviso, e ele é o mais importante de alcançar.
CREATE INDEX IF NOT EXISTS idx_carts_erp_order_status_stale
    ON carts (erp_order_status_at, created_at)
    WHERE external_order_id IS NOT NULL
      AND (erp_order_status IS NULL
           OR erp_order_status NOT IN ('entregue', 'cancelado', 'nao_entregue'));

CREATE TABLE IF NOT EXISTS erp_order_status_events (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id          UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    -- Nulo é possível: um pedido criado no ERP por fora do LiveCart também
    -- dispara webhook, e registrar a passagem é melhor do que descartá-la.
    cart_id           UUID REFERENCES carts(id) ON DELETE CASCADE,
    external_order_id VARCHAR(64) NOT NULL,
    order_number      VARCHAR(32),
    status            VARCHAR(32) NOT NULL,
    -- Nulo na primeira observação do pedido.
    previous_status   VARCHAR(32),
    -- 'webhook' quando o ERP avisou, 'sweep' quando fomos nós que perguntamos.
    -- A diferença importa: uma linha 'sweep' é a prova de que um webhook se
    -- perdeu, e várias delas seguidas apontam para a URL descadastrada.
    source            VARCHAR(16) NOT NULL,
    observed_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload           JSONB
);

CREATE INDEX IF NOT EXISTS idx_erp_order_status_events_cart
    ON erp_order_status_events (cart_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_erp_order_status_events_store
    ON erp_order_status_events (store_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_erp_order_status_events_order
    ON erp_order_status_events (external_order_id, observed_at DESC);
