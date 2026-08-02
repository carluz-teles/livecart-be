-- D15 + N2 — a whitelist de produtos passa a ser da SESSÃO. Espelha
-- event_products (000037_event_products_upsells.up.sql:7-18) coluna por coluna.
--
-- Semântica ÚNICA em todo o sistema (N2): lista vazia = TODOS os produtos da
-- loja liberados naquela sessão. Hoje isso vale só no checkout; a ingestão de
-- post/story faz o OPOSTO (lista vazia bloqueia tudo e responde 'not_in_promo').
-- Quem alinha os dois é o código, não esta migration.
--
-- EXPAND: event_products NÃO é removida aqui. A remoção é a 000119.

CREATE TABLE session_products (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id    UUID NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
    product_id    UUID NOT NULL REFERENCES products(id)      ON DELETE CASCADE,
    special_price INTEGER,
    max_quantity  INTEGER,
    display_order INTEGER NOT NULL DEFAULT 0,
    featured      BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, product_id)
);

CREATE INDEX idx_session_products_session ON session_products (session_id);
CREATE INDEX idx_session_products_product ON session_products (product_id);
CREATE INDEX idx_session_products_order   ON session_products (session_id, display_order);

-- Cópia da whitelist do evento para TODAS as sessões daquele evento (N2).
-- A base de produção é ZERADA antes do épico, então aqui isso é um no-op — mas
-- em desenvolvimento e staging é o que impede que toda barreira de produto
-- configurada desapareça no dia do deploy (pela semântica nova, "sumir" não é
-- erro visível: é "passou a vender tudo").
-- Evento SEM sessão não gera linha e perde a lista: esse estado existe hoje
-- (live/service.go registra "live created without session").
INSERT INTO session_products
    (session_id, product_id, special_price, max_quantity, display_order, featured, created_at, updated_at)
SELECT ls.id, ep.product_id, ep.special_price, ep.max_quantity,
       ep.display_order, ep.featured, ep.created_at, now()
FROM event_products ep
JOIN live_sessions ls ON ls.event_id = ep.event_id
ON CONFLICT (session_id, product_id) DO NOTHING;

COMMENT ON TABLE session_products IS
    'D15/N2: whitelist de produtos por SESSÃO. Lista vazia = TODOS os produtos da loja liberados naquela sessão — mesma semântica no checkout e na ingestão, sem exceção. Sessão nova nasce vazia, portanto vende tudo, mesmo que outra sessão do evento tenha whitelist.';
