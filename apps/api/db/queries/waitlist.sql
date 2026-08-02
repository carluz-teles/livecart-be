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
  AND status IN ('waiting', 'notified');

-- name: ExpireWaitlistByEvent :many
-- RN-32 — no fim do evento + carência, o item de fila NÃO ATENDIDO morre.
--
-- Sem isto o carrinho do waitlister é ETERNO. O guard do ExpireCart se abstém
-- de expirar qualquer carrinho com item 'waiting', e o ramo 'waiting' não tem
-- prazo nenhum: enquanto o item estiver na fila o carrinho inteiro é
-- inexpirável, segurando o estoque reservado dos seus itens NÃO-waitlisted. E
-- como não existe mais sweep de carrinhos, a task cart.expire dispara uma vez,
-- encontra 0 rows e encerra — nada mais a re-arma, exceto uma promoção que por
-- definição não vem (o carrinho da frente foi PAGO em vez de expirar).
--
-- O predicado é mais estreito do que o desta query antes desta mudança. Ela
-- existia desde a 000026 sem nenhum chamador Go, com
-- `status IN ('waiting','notified')` — matando também quem acabou de ser
-- promovido e ainda está DENTRO da janela de TTL válida, o que o PRD proíbe
-- explicitamente. Quem está 'notified' com janela futura tem o expires_at do
-- carrinho estendido e é governado pelo próprio cart.expire: quando a janela
-- vencer, o guard do ExpireCart deixa de vetar e o carrinho expira sozinho.
--
-- Devolve os cart_id afetados para o chamador RE-ARMAR cart.expire neles: o
-- prazo deles já venceu enquanto o guard vetava, então sem o re-arm eles
-- continuariam vivos mesmo com a fila morta.
UPDATE waitlist_items
SET status = 'expired'
WHERE event_id = $1
  AND (
      status = 'waiting'
      OR (status = 'notified' AND expires_at IS NOT NULL AND expires_at <= now())
  )
RETURNING cart_id;

-- name: ListExpiredNotifiedWaitlistItems :many
SELECT * FROM waitlist_items
WHERE status = 'notified' AND expires_at IS NOT NULL AND expires_at < now();

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
-- Itens em fila (waiting/notified) vinculados ao carrinho do cliente —
-- alimenta a seção de waitlist no /cart/:token.
SELECT wi.*,
       p.name      AS product_name,
       p.keyword   AS product_keyword,
       p.image_url AS product_image_url,
       p.price     AS product_price
FROM waitlist_items wi
JOIN products p ON p.id = wi.product_id
WHERE wi.cart_id = $1 AND wi.status IN ('waiting','notified')
ORDER BY wi.created_at;

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
