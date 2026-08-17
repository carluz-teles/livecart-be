-- =============================================================================
-- LIVE COMMENTS
-- =============================================================================

-- name: CreateLiveComment :one
INSERT INTO live_comments (
    session_id, event_id, platform, platform_comment_id,
    platform_user_id, platform_handle, text,
    has_purchase_intent, matched_product_id, matched_quantity, result
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: UpdateLiveCommentResult :exec
UPDATE live_comments
SET has_purchase_intent = $2,
    matched_product_id = $3,
    matched_quantity = $4,
    result = $5
WHERE id = $1;

-- name: ListCommentsBySession :many
SELECT * FROM live_comments
WHERE session_id = $1
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: ListCommentsByEvent :many
SELECT * FROM live_comments
WHERE event_id = $1
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: ListCommentsByUser :many
SELECT * FROM live_comments
WHERE event_id = $1 AND platform_user_id = $2
ORDER BY created_at;

-- name: CountCommentsBySession :one
SELECT COUNT(*)::int FROM live_comments WHERE session_id = $1;

-- name: CountCommentsByEvent :one
SELECT COUNT(*)::int FROM live_comments WHERE event_id = $1;

-- name: ListPurchaseCommentsByEvent :many
SELECT lc.*, p.name AS product_name, p.keyword AS product_keyword
FROM live_comments lc
LEFT JOIN products p ON p.id = lc.matched_product_id
WHERE lc.event_id = $1 AND lc.has_purchase_intent = true
ORDER BY lc.created_at;

-- name: FindCommentCartCorrelation :one
-- Telemetria (Fatia 4/6): correlaciona um comentário com a ADIÇÃO de item ao
-- carrinho do mesmo comprador+evento, dentro de uma janela de 120s a partir
-- do comentário — alimenta LiveCommerceComment{converted_to_cart}.
--
-- Casa por cart_item_events.created_at (não carts.created_at). GetOrCreateCart
-- (internal/live/repository.go) reusa o carrinho ABERTO do comprador no
-- evento: só o comentário que CRIA o carrinho cai dentro da janela de
-- carts.created_at; comentários seguintes do mesmo comprador que adicionam
-- outro produto ao carrinho JÁ ABERTO ficam fora dela, porque o carrinho é
-- bem mais velho que o comentário. cart_item_events tem uma linha por
-- adição (RN-12, migration 000110), com created_at próprio — casar por ela
-- cobre os dois casos (carrinho novo e carrinho reaberto) com a mesma regra.
--
-- LEFT JOIN carts primeiro (todo carrinho do comprador no evento, aberto ou
-- arquivado) e depois LEFT JOIN cart_item_events filtrado pela janela: cie é
-- quem decide o match, não ct — por isso cart_id vem de cie.cart_id, não de
-- ct.id. Se nenhuma adição cair na janela, cie.cart_id é NULL mesmo que o
-- comprador tenha carrinho (aberto ou não) — comment.converted_to_cart=false
-- nesse caso. ORDER BY/LIMIT: quando há mais de uma adição na janela (raro,
-- ex.: dois produtos no mesmo comentário), pega a mais antiga.
SELECT
    lc.id                AS comment_id,
    lc.event_id,
    le.store_id,
    lc.session_id,
    lc.platform,
    lc.platform_user_id,
    lc.has_purchase_intent,
    lc.matched_product_id,
    lc.result,
    lc.created_at         AS comment_created_at,
    cie.cart_id           AS cart_id,
    cie.created_at        AS item_created_at
FROM live_comments lc
JOIN live_events le ON le.id = lc.event_id
LEFT JOIN carts ct
    ON ct.event_id = lc.event_id
   AND ct.platform_user_id = lc.platform_user_id
LEFT JOIN cart_item_events cie
    ON cie.cart_id = ct.id
   AND cie.created_at BETWEEN lc.created_at AND lc.created_at + INTERVAL '120 seconds'
WHERE lc.platform_comment_id = $1
ORDER BY lc.created_at DESC, cie.created_at ASC NULLS LAST
LIMIT 1;
