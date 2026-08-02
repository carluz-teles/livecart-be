-- =============================================================================
-- SESSION PRODUCTS (Whitelist da SESSÃO — D15/N2)
--
-- Regra única: lista vazia = TODOS os produtos da loja liberados.
--
-- Só existe aqui o que tem chamador. As seis queries de event_product.sql que
-- nunca tiveram um (HasEventProducts, IsProductInEventWhitelist,
-- GetEffectiveProductPrice, DeleteAllEventProducts, ListFeaturedEventProducts,
-- DeleteEventProductByProductID) NÃO foram espelhadas.
--
-- O CRUD é chaveado por PRODUCT_ID, não pelo id da linha: o par FE/BE de hoje
-- está quebrado justamente porque o frontend manda products.id onde a API
-- espera event_products.id (PUT devolve 404, DELETE apaga zero linhas e ainda
-- responde 200).
-- =============================================================================

-- name: UpsertSessionProduct :one
INSERT INTO session_products (session_id, product_id, special_price, max_quantity, display_order, featured)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (session_id, product_id) DO UPDATE SET
    special_price = EXCLUDED.special_price,
    max_quantity  = EXCLUDED.max_quantity,
    display_order = EXCLUDED.display_order,
    featured      = EXCLUDED.featured,
    updated_at    = now()
RETURNING *;

-- name: GetSessionProductByProductID :one
SELECT
    sp.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM session_products sp
JOIN products p ON p.id = sp.product_id
WHERE sp.session_id = $1 AND sp.product_id = $2;

-- name: ListSessionProducts :many
SELECT
    sp.*,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM session_products sp
JOIN products p ON p.id = sp.product_id
WHERE sp.session_id = $1
ORDER BY sp.display_order ASC, sp.created_at ASC;

-- name: DeleteSessionProduct :exec
DELETE FROM session_products WHERE session_id = $1 AND product_id = $2;

-- name: CountSessionProducts :one
SELECT COUNT(*)::int FROM session_products WHERE session_id = $1;

-- name: DeleteSessionProductsByEventAndProduct :exec
-- Remove o produto da whitelist de TODAS as sessões do evento. Sustenta a rota
-- legada por evento, que o frontend ainda usa.
DELETE FROM session_products sp
USING live_sessions ls
WHERE ls.id = sp.session_id AND ls.event_id = $1 AND sp.product_id = $2;

-- name: UpsertSessionProductForEvent :exec
-- Aplica a whitelist do EVENTO em todas as sessões existentes dele. É a
-- tradução da rota legada por evento para o modelo novo. Sessão criada DEPOIS
-- nasce vazia (N2, decisão explícita do dono) — não há herança.
INSERT INTO session_products (session_id, product_id, special_price, max_quantity, display_order, featured)
SELECT ls.id, $2, $3, $4, $5, $6
FROM live_sessions ls
WHERE ls.event_id = $1
ON CONFLICT (session_id, product_id) DO UPDATE SET
    special_price = EXCLUDED.special_price,
    max_quantity  = EXCLUDED.max_quantity,
    display_order = EXCLUDED.display_order,
    featured      = EXCLUDED.featured,
    updated_at    = now();

-- name: ListEventWhitelistFromSessions :many
-- União das whitelists das sessões do evento, UMA linha por produto. Serve a
-- listagem legada por evento e o badge de contagem.
-- DISTINCT ON, não GROUP BY: o produto pode estar em várias sessões com preços
-- diferentes, e quem vale para o comprador é o MENOR preço especial.
SELECT * FROM (
    SELECT DISTINCT ON (sp.product_id)
        sp.id,
        sp.session_id,
        sp.product_id,
        sp.special_price,
        sp.max_quantity,
        sp.display_order,
        sp.featured,
        sp.created_at,
        sp.updated_at,
        p.name AS product_name,
        p.keyword AS product_keyword,
        p.price AS original_price,
        p.image_url AS product_image_url,
        p.stock AS product_stock,
        p.active AS product_active
    FROM session_products sp
    JOIN live_sessions ls ON ls.id = sp.session_id
    JOIN products p ON p.id = sp.product_id
    WHERE ls.event_id = $1
    ORDER BY sp.product_id, sp.special_price ASC NULLS LAST, sp.display_order ASC, sp.created_at ASC
) w
ORDER BY w.display_order ASC, w.created_at ASC;

-- name: GetEventProductConfigFromSessions :one
-- Config do produto para validação no CHECKOUT.
--
-- O carrinho é do EVENTO e atravessa N sessões — não existe "a sessão do
-- checkout". A regra é a união: o produto é aceito se ALGUMA sessão do evento o
-- aceita, e uma sessão sem whitelist aceita tudo (N2). Evento sem sessão
-- nenhuma também aceita tudo, senão o carrinho ficaria intransponível.
SELECT
    p.id AS product_id,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.stock AS product_stock,
    p.active AS product_active,
    w.special_price,
    w.max_quantity,
    COALESCE(w.special_price, p.price)::bigint AS effective_price,
    (
        NOT EXISTS (SELECT 1 FROM live_sessions ls WHERE ls.event_id = $1)
        OR EXISTS (
            SELECT 1 FROM live_sessions ls
            WHERE ls.event_id = $1
              AND (
                  NOT EXISTS (SELECT 1 FROM session_products sp2 WHERE sp2.session_id = ls.id)
                  OR EXISTS (
                      SELECT 1 FROM session_products sp3
                      WHERE sp3.session_id = ls.id AND sp3.product_id = p.id
                  )
              )
        )
    ) AS is_allowed
FROM products p
LEFT JOIN LATERAL (
    -- O menor preço especial e o maior teto entre as sessões que listam o
    -- produto: a regra de união é "alguma sessão aceita".
    SELECT sp.special_price,
           sp.max_quantity
    FROM session_products sp
    JOIN live_sessions ls ON ls.id = sp.session_id
    WHERE ls.event_id = $1 AND sp.product_id = p.id
    ORDER BY sp.special_price ASC NULLS LAST, sp.max_quantity DESC NULLS LAST
    LIMIT 1
) w ON true
WHERE p.id = $2 AND p.store_id = $3;

-- name: GetEffectiveMaxQuantityFromSessions :one
-- Teto efetivo: whitelist da sessão (o maior entre as sessões, pela regra de
-- união) > evento > loja > 5.
SELECT COALESCE(
    (SELECT MAX(sp.max_quantity)
     FROM session_products sp
     JOIN live_sessions ls ON ls.id = sp.session_id
     WHERE ls.event_id = e.id AND sp.product_id = $2),
    e.cart_max_quantity_per_item,
    s.cart_max_quantity_per_item,
    5
)::int AS max_quantity
FROM live_events e
JOIN stores s ON s.id = e.store_id
WHERE e.id = $1;

-- name: GetEventWhitelistProduct :one
-- Uma linha da união por evento, para devolver o produto recém-gravado pela
-- rota legada sem carregar a lista inteira.
SELECT
    sp.id,
    sp.session_id,
    sp.product_id,
    sp.special_price,
    sp.max_quantity,
    sp.display_order,
    sp.featured,
    sp.created_at,
    sp.updated_at,
    p.name AS product_name,
    p.keyword AS product_keyword,
    p.price AS original_price,
    p.image_url AS product_image_url,
    p.stock AS product_stock,
    p.active AS product_active
FROM session_products sp
JOIN live_sessions ls ON ls.id = sp.session_id
JOIN products p ON p.id = sp.product_id
WHERE ls.event_id = $1 AND sp.product_id = $2
ORDER BY sp.special_price ASC NULLS LAST, sp.display_order ASC, sp.created_at ASC
LIMIT 1;

-- name: CountEventWhitelistFromSessions :one
-- Quantos produtos DISTINTOS o evento barra ao todo. Alimenta o badge da aba
-- "Produtos", que perdeu a fonte única quando a whitelist desceu para a sessão.
SELECT COUNT(DISTINCT sp.product_id)::int
FROM session_products sp
JOIN live_sessions ls ON ls.id = sp.session_id
WHERE ls.event_id = $1;
