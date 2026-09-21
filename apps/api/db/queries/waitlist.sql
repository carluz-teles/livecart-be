-- =============================================================================
-- WAITLIST ITEMS
-- =============================================================================

-- name: CreateWaitlistItem :one
INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, cart_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetNextWaitlistPosition :one
SELECT COALESCE(MAX(position), 0) + 1 AS next_position
FROM waitlist_items
WHERE event_id = $1 AND product_id = $2;

-- name: GetFirstWaitingByProduct :one
SELECT * FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting'
ORDER BY position ASC
LIMIT 1;

-- name: UpdateWaitlistItemStatus :exec
UPDATE waitlist_items
SET status = $2,
    notified_at = $3,
    fulfilled_at = $4,
    expires_at = $5
WHERE id = $1;

-- name: ListWaitlistByEventAndUser :many
SELECT wi.*, p.name AS product_name, p.keyword AS product_keyword, p.image_url AS product_image_url
FROM waitlist_items wi
JOIN products p ON p.id = wi.product_id
WHERE wi.event_id = $1 AND wi.platform_user_id = $2
ORDER BY wi.created_at;

-- name: ListWaitlistByEventAndProduct :many
SELECT * FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting'
ORDER BY position ASC;

-- name: GetWaitlistItemByEventUserProduct :one
SELECT * FROM waitlist_items
WHERE event_id = $1 AND platform_user_id = $2 AND product_id = $3
  AND status IN ('waiting', 'notified')
ORDER BY created_at,queue_sequence LIMIT 1;

-- name: ExpireWaitlistByEvent :many
-- The cart owns the only deadline. Lock carts before changing their waiting
-- balances, using the same order as payment, promotion and cart expiration.
WITH due_carts AS MATERIALIZED (
    SELECT c.id FROM carts c
    WHERE c.status IN ('active', 'checkout', 'expired')
      AND NOT c.never_expires
      AND NOT c.payment_review_required
      AND c.payment_status IS DISTINCT FROM 'paid'
      AND c.payment_status IS DISTINCT FROM 'refunded'
      AND c.expires_at <= now()
      AND EXISTS (
          SELECT 1 FROM waitlist_items wi
          WHERE wi.cart_id = c.id AND wi.event_id = $1 AND wi.status = 'waiting'
      )
    ORDER BY c.id
    FOR UPDATE OF c
), expired AS (
    UPDATE waitlist_items wi
    SET status = 'expired', cancelled_at = COALESCE(wi.cancelled_at, now()),
        cancelled_quantity = wi.cancelled_quantity + wi.quantity, quantity = 0
    FROM due_carts c
    WHERE wi.cart_id = c.id AND wi.event_id = $1 AND wi.status = 'waiting'
    RETURNING wi.id, wi.cart_id, wi.platform_user_id, wi.platform_handle, wi.product_id
)
SELECT
    e.cart_id,
    e.platform_user_id,
    e.platform_handle,
    p.name AS product_name,
    c.token AS cart_token
FROM expired e
JOIN products p ON p.id = e.product_id
LEFT JOIN carts c ON c.id = e.cart_id;

-- name: ListExpiredNotifiedWaitlistItems :many
-- Backward-compatible reader: promoted units have no independent deadline.
SELECT wi.* FROM waitlist_items wi WHERE FALSE;

-- name: CountWaitingByProduct :one
SELECT COUNT(*)::int FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status = 'waiting';

-- name: CountActiveByEventProduct :one
-- waiting + notified — usado pelo webhook ERP para decidir se vale tentar
-- promover alguém após uma mudança de saldo.
SELECT COUNT(*)::int FROM waitlist_items
WHERE event_id = $1 AND product_id = $2 AND status IN ('waiting','notified');

-- name: ListEventsWithWaitingByProduct :many
-- Eventos distintos que têm pelo menos um cliente em status='waiting' para
-- o produto. Usado pelo webhook Tiny para varrer e promover. Filtramos só
-- 'waiting' porque 'notified' já tem stock alocado — não precisa promover
-- o mesmo evento de novo.
SELECT DISTINCT event_id
FROM waitlist_items
WHERE product_id = $1 AND status = 'waiting';

-- name: ListActiveByCart :many
-- Position means eligible requests ahead in the product's store-wide FIFO,
-- not the historical counter of one event. Paid/expired hosts do not count.
WITH queued AS (
    SELECT wi.id,(row_number() OVER(PARTITION BY wi.product_id ORDER BY wi.created_at,wi.queue_sequence))::int AS queue_position
    FROM waitlist_items wi JOIN carts c ON c.id=wi.cart_id
    JOIN live_events e ON e.id=c.event_id JOIN products p ON p.id=wi.product_id
    LEFT JOIN carts host ON host.id=c.joined_to_cart_id
    WHERE wi.status='waiting' AND wi.quantity>0 AND p.store_id=e.store_id
      AND NOT c.purchase_closed AND c.status IN ('active','checkout')
      AND c.payment_status IS DISTINCT FROM 'paid' AND c.payment_status IS DISTINCT FROM 'refunded'
      AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>now())
      AND (host.id IS NULL OR (host.status IN ('active','checkout')
        AND host.payment_status IS DISTINCT FROM 'paid' AND host.payment_status IS DISTINCT FROM 'refunded'
        AND (host.never_expires OR host.expires_at IS NULL OR host.expires_at>now())))
      AND wi.product_id IN (SELECT own.product_id FROM waitlist_items own WHERE own.cart_id=$1 AND own.status='waiting')
)
SELECT wi.*,queued.queue_position,
       p.name AS product_name,p.keyword AS product_keyword,p.image_url AS product_image_url,
       wi.unit_price AS product_price
FROM waitlist_items wi JOIN queued ON queued.id=wi.id
JOIN products p ON p.id=wi.product_id
WHERE wi.cart_id=$1
ORDER BY wi.created_at,wi.queue_sequence;

-- name: CancelWaitlistItem :exec
-- cart_id no WHERE garante ownership (cliente só consegue cancelar itens
-- do próprio carrinho); status no WHERE evita atropelar fulfilled/expired.
UPDATE waitlist_items
SET status = 'cancelled', cancelled_at = now()
WHERE id = $1 AND cart_id = $2 AND status IN ('waiting','notified');

-- name: CancelWaitlistItemsByCart :many
-- Mata a fila do cart inteiro. Usado pelo cancelamento manual do lojista: o
-- carrinho deixa de existir para o comprador, então mantê-lo na fila só geraria
-- promoção (e DM) para um checkout morto — o bug G1 da auditoria, agora fechado
-- na origem. Só toca itens ainda vivos; fulfilled/expired/cancelled ficam como
-- estão, o que torna a chamada idempotente.
UPDATE waitlist_items
SET status = 'cancelled', cancelled_at = now()
WHERE cart_id = $1 AND status IN ('waiting', 'notified')
RETURNING *;

-- name: CancelWaitlistItemsByCartAndProduct :many
-- Mata a fila de UM produto do carrinho. É o que faltava quando o comprador
-- reduz a quantidade no checkout: a parcela em fila é a primeira a sair
-- (splitQuantityChange), mas a LINHA em waitlist_items continuava viva.
--
-- Uma linha órfã dessas é reivindicada pela próxima promoção, que debita
-- estoque local, emite uma SAÍDA no Tiny e não entrega unidade a ninguém — o
-- comprador já tinha desistido daquela parcela. É o gerador crônico do sintoma
-- "o comprador tira uma unidade e o sistema devolve errado".
--
-- Só toca itens vivos, então repetir a chamada é inofensivo.
UPDATE waitlist_items
SET status = 'cancelled', cancelled_at = now()
WHERE cart_id = $1 AND product_id = $2 AND status IN ('waiting', 'notified')
RETURNING *;

-- name: GetWaitlistItemForCart :one
SELECT * FROM waitlist_items
WHERE id = $1 AND cart_id = $2;

-- name: MarkWaitlistNotified :exec
-- Promove o cliente da fila para "notified" com a janela de TTL extra.
UPDATE waitlist_items
SET status               = 'notified',
    notified_at          = now(),
    expires_at           = $2,
    notification_sent_at = $3
WHERE id = $1;

-- name: MarkWaitlistFulfilledByCart :many
-- Chamado no callback OnCartPaid: tudo que estava notified naquele cart
-- vira fulfilled (cliente pagou dentro da janela). Retorna as rows afetadas
-- para emitir um waitlist.fulfilled por item (keyed by waitlist_item_id).
UPDATE waitlist_items
SET status = 'fulfilled', fulfilled_at = now()
WHERE cart_id = $1 AND status = 'notified'
RETURNING id, product_id, event_id;

-- name: ListNotifiedByCart :many
-- Carts pagos: precisamos saber quais notified existem para marcar como
-- fulfilled e logar (não é estritamente necessário porque
-- MarkWaitlistFulfilledByCart cobre — usado em testes/observabilidade).
SELECT * FROM waitlist_items
WHERE cart_id = $1 AND status = 'notified';

-- name: ClaimNextWaitlistItem :one
-- Reivindica ATOMICAMENTE o próximo da fila (menor position) e o marca
-- 'notified' na MESMA transação. FOR UPDATE SKIP LOCKED garante que callers
-- concorrentes reivindicam clientes DISTINTOS — nunca o mesmo. Corrige a
-- promoção desperdiçada quando N unidades são liberadas ao mesmo tempo: sem
-- isso, dois callers pegavam o mesmo W1 e consumiam 2 unidades avançando a
-- fila só 1 (o próximo ficava preso com uma unidade "perdida").
UPDATE waitlist_items
SET status               = 'notified',
    notified_at          = now(),
    expires_at           = $3,
    notification_sent_at = now()
WHERE id = (
    SELECT wi.id FROM waitlist_items wi
    WHERE wi.event_id = $1 AND wi.product_id = $2 AND wi.status = 'waiting'
    ORDER BY wi.position ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: RevertWaitlistToWaiting :exec
-- Desfaz uma reivindicação quando o gate de estoque falha DEPOIS do claim
-- (não havia unidade para este cliente) — devolve-o ao topo da fila.
UPDATE waitlist_items
SET status               = 'waiting',
    notified_at          = NULL,
    expires_at           = NULL,
    notification_sent_at = NULL
WHERE id = $1 AND status = 'notified';

-- name: RequeueWaitlistItemPartial :exec
-- Promoção PARCIAL: o cliente recebeu parte do pedido; a entry volta para a
-- fila (mesma posição) aguardando o restante. Limpa os campos de notificação
-- porque ela deixa de estar 'notified'.
UPDATE waitlist_items
SET quantity             = $2,
    status               = 'waiting',
    notified_at          = NULL,
    expires_at           = NULL,
    notification_sent_at = NULL
WHERE id = $1;
