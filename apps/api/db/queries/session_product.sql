-- =============================================================================
-- SESSION PRODUCTS — a lista de produtos vendáveis é da TRANSMISSÃO
--
-- Regra única, decidida pelo dono do produto: a lista pertence à SESSÃO e SÓ a
-- ela. Uma live pode vender qualquer coisa, um post vende só o produto X e um
-- story só o produto Y — e os três podem ser transmissões da MESMA campanha, ao
-- mesmo tempo. Por isso não existe (mais) lista no nível do EVENTO: nem query,
-- nem rota, nem contagem.
--
--   lista VAZIA = TODOS os produtos ativos da loja liberados naquela transmissão.
--   sessão nova NASCE VAZIA — não há herança, porque não há de onde herdar.
--
-- A herança (InheritEventWhitelistIntoSession) e a escrita por evento
-- (UpsertSessionProductForEvent / DeleteSessionProductsByEventAndProduct /
-- ListEventWhitelistFromSessions / GetEventWhitelistProduct /
-- CountEventWhitelistFromSessions) saíram junto: existiam para o problema
-- "configurei na campanha e criei a sessão depois", que deixa de existir quando
-- não há lista de campanha e cada transmissão é configurada explicitamente.
--
-- O CHECKOUT continua na UNIÃO (GetEventProductConfigFromSessions, no fim deste
-- arquivo) porque o carrinho é do EVENTO e atravessa N transmissões: não existe
-- "a sessão do checkout".
--
-- O CRUD é chaveado por PRODUCT_ID, não pelo id da linha: chavear pelo id da
-- linha foi o que quebrou o par FE/BE (o frontend manda products.id onde a API
-- esperava o id da linha — PUT devolvia 404 e DELETE apagava zero linhas
-- respondendo 200).
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

-- name: CountSessionProductsByEvent :many
-- Quantos produtos cada transmissão da campanha libera. UMA leitura por evento,
-- não uma por sessão: o detalhe da campanha lista todas as transmissões e o
-- laço com CountSessionProducts seria N+1.
--
-- Só devolve linha para sessão QUE TEM produto: contagem zero é a ausência da
-- linha, e zero é a resposta legítima "esta transmissão vende tudo".
SELECT sp.session_id, COUNT(*)::int AS product_count
FROM session_products sp
JOIN live_sessions ls ON ls.id = sp.session_id
WHERE ls.event_id = $1
GROUP BY sp.session_id;

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


-- name: CountProductsOwnedByStore :one
-- Quantos dos ids informados pertencem MESMO a esta loja.
--
-- A whitelist da transmissão grava (session_id, product_id) e confia na FK, que
-- so garante que o produto EXISTE — nao que ele e do lojista que esta pedindo.
-- Enquanto a lista so entrava um a um por rota autenticada isso passava; expor
-- productIds na criacao da sessao amplia a superficie, e um uuid de outra loja
-- entraria na lista de venda desta.
--
-- Contar e comparar com o tamanho da lista responde a pergunta inteira numa ida
-- ao banco: se o numero bate, todos sao dele; se nao bate, pelo menos um nao e —
-- e nao interessa qual, porque a resposta e a mesma.
SELECT count(*) FROM products
WHERE store_id = sqlc.arg(store_id)
  AND id = ANY(sqlc.arg(product_ids)::uuid[]);
