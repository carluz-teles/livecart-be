-- Razão de movimentos de estoque contra o ERP.
--
-- Até aqui, o registro de uma reserva só nascia DEPOIS de a Tiny responder
-- (stock_reservations ganha a linha com o erp_movement_id em mãos). Qualquer
-- falha entre "decidimos reservar" e "a Tiny confirmou" não deixava rastro
-- nenhum — e timeout é ambíguo: na live de 17/08/2026 dois timeouts idênticos
-- tiveram desfechos opostos (um lançamento entrou na Tiny, o outro não), e a
-- API v3 não oferece consulta de lançamentos para desempatar depois.
--
-- Esta tabela é a intenção, gravada ANTES da chamada. Uma linha por POST de
-- estoque; a linha nunca é apagada, só muda de estado:
--
--   pending      chamada em voo (ou processo morreu no meio dela)
--   confirmed    a Tiny devolveu o idLancamento
--   failed       provado que NÃO chegou (falha de discagem, recusa 4xx) —
--                seguro re-executar
--   unconfirmed  ambíguo (timeout, 5xx, resposta ilegível) — NUNCA re-executado
--                às cegas; fica visível e trava a finalização do carrinho até
--                alguém (worker com prova, ou humano pelo extrato) decidir
--   resolving    reivindicado por um resolver; volta ao jogo se envelhecer
--
-- A idempotency_key viaja na observação do lançamento na Tiny: é o que permite
-- a um humano casar linha daqui com lançamento de lá no extrato do produto.
CREATE TABLE erp_stock_movements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id UUID NOT NULL,
    cart_id UUID NOT NULL,
    event_id UUID,
    product_id UUID NOT NULL,
    external_product_id VARCHAR NOT NULL,
    direction VARCHAR NOT NULL CHECK (direction IN ('out', 'in')),
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    unit_price_cents BIGINT NOT NULL DEFAULT 0,
    idempotency_key UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    status VARCHAR NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'failed', 'unconfirmed', 'resolving')),
    erp_movement_id VARCHAR,
    attempts SMALLINT NOT NULL DEFAULT 0,
    last_error TEXT,
    last_attempt_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- O resolver varre só o que não está resolvido; parcial mantém o índice pequeno
-- num razão que só cresce.
CREATE INDEX idx_erp_stock_movements_unresolved
    ON erp_stock_movements(created_at)
    WHERE status IN ('pending', 'failed', 'unconfirmed', 'resolving');

-- O gate da finalização pergunta por carrinho.
CREATE INDEX idx_erp_stock_movements_cart ON erp_stock_movements(cart_id);
