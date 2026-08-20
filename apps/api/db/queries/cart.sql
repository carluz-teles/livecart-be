-- =============================================================================
-- CARTS (belong to events, with optional session tracking)
-- =============================================================================

-- name: CreateCart :one
INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, status, expires_at, customer_id, short_id)
VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $8)
RETURNING *;

-- name: ExtendCartExpiration :exec
-- Empurra expires_at para no mínimo @new_expires_at ("gordura" extra para
-- clientes promovidos da waitlist). GREATEST evita encolher o prazo se o
-- cart já tem um expires_at maior do que o pedido.
--
-- CARRINHO SEM PRAZO NÃO GANHA UM AQUI. expires_at NULL é a RN-04: o carrinho
-- não expira enquanto o evento roda. É o prazo mais LONGO que existe, não a
-- ausência de prazo — e era exatamente ao contrário que esta query o tratava.
--
-- O COALESCE que estava aqui virava NULL no valor novo, e o GREATEST comparava
-- o valor novo com ele mesmo: promover alguém da fila CRIAVA um prazo de 30
-- minutos num carrinho que tinha até o fim da campanha. Aconteceu em staging —
-- o comprador foi promovido às 14:10, nunca recebeu o aviso (a DM falhou),
-- adicionou outro produto às 14:39 e perdeu o carrinho inteiro às 14:40, com o
-- evento aberto até dois dias depois.
--
-- A intenção da regra é dar tempo A MAIS a quem esperou. Encurtar era o oposto.
-- O prazo da FILA continua existindo e continua vencendo: ele mora na linha da
-- waitlist (notified_until) e é ExpireNotifiedWaitlistItem quem o aplica —
-- devolvendo o item ao estoque, não matando o carrinho do comprador.
UPDATE carts
SET expires_at = GREATEST(expires_at, @new_expires_at::timestamptz)
WHERE id = $1
  AND expires_at IS NOT NULL;

-- name: UpdateCartShippingAddress :exec
-- Replaces the cart's shipping_address JSONB. Used by the admin's "edit
-- address" action — narrower than UpdateCartCustomerCheckout (which also
-- writes customer fields) so we don't accidentally clear contact info.
UPDATE carts
SET shipping_address = $2
WHERE id = $1;

-- RegenerateCartCheckout foi REMOVIDA: nunca teve chamador Go (o "regerar link"
-- do painel usa order/repository.RegenerateCheckout) e ainda devolvia o carrinho
-- para status='active' com expires_at no futuro — o estado que a RN-06 declara
-- impossível. Portá-la seria carregar código morto E errado.

-- name: IssueShortIDForEvent :one
-- Atomically issues the next short_id for the store that owns the given event.
-- On first call for a store, INSERT seeds last_value at 1000 (the chosen
-- starting number). On subsequent calls, the ON CONFLICT UPDATE bumps
-- last_value by 1 and returns the new value. Either way the RETURNING clause
-- gives the caller the short_id it should write into the cart row in the same
-- transaction. Accepts event_id so callers don't need to resolve store_id
-- themselves.
INSERT INTO store_order_counters (store_id, last_value)
SELECT e.store_id, 1000 FROM live_events e WHERE e.id = $1
ON CONFLICT (store_id) DO UPDATE SET last_value = store_order_counters.last_value + 1
RETURNING last_value;

-- name: GetCartByID :one
SELECT * FROM carts WHERE id = $1;

-- name: GetCartByToken :one
SELECT * FROM carts WHERE token = $1;

-- Fatia 10-a: o tracking_token pós-venda saiu do cart. A fonte da verdade é
-- order_logistics.tracking_token (Fatia C1); a escrita usa
-- SetOrderLogisticsTrackingToken e o lookup público, GetCartByOrderLogisticsTrackingToken
-- (ambos em order_write.sql).

-- name: GetCartByEventAndUser :one
-- O carrinho ABERTO do comprador neste evento.
--
-- Desde a 000107 pode existir mais de um carrinho por (evento, comprador): pagar
-- ou expirar libera a vaga e um novo nasce (RN-07/RN-08). Sem o filtro abaixo,
-- `:one` gera QueryRow do pgx, que lê a PRIMEIRA linha e descarta o resto SEM
-- erro — o item do comprador cairia num carrinho arbitrário, possivelmente o já
-- pago. O ORDER BY é a desempate determinística caso o filtro deixe passar mais
-- de um (não deveria: o índice parcial garante no máximo um).
SELECT * FROM carts
WHERE event_id = $1 AND platform_user_id = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at DESC
LIMIT 1;

-- name: ListCartsByCustomer :many
-- Returns all carts for a specific customer with totals
SELECT
    c.*,
    cart_product_total_cents(c.id) AS total_value,
    COALESCE(SUM(ci.quantity), 0)::int AS total_items
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.customer_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateCartCustomerID :exec
UPDATE carts SET customer_id = $2 WHERE id = $1;

-- name: GetCartTotals :one
-- Returns total items and value for a cart (for notifications)
SELECT
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    cart_product_total_cents($1)::bigint AS total_value
FROM cart_items ci
WHERE ci.cart_id = $1;

-- name: UpdateCartStatus :one
UPDATE carts SET status = $2 WHERE id = $1 RETURNING *;

-- name: ExpireCart :one
-- Flip idempotente e guard-first do worker de expiração. O guard vive DENTRO do
-- UPDATE para fechar a corrida com o webhook de pagamento: se alguém pagou ou o
-- cart já foi expirado/cancelado no intervalo, 0 rows retornam e o caller ABORTA
-- sem devolver estoque nem tocar o ERP. Marcar 'expired' é a PRIMEIRA ação (no
-- mesmo tx da devolução de estoque local) — a ação irreversível de ERP só roda
-- depois que o cart está comprovadamente 'expired'.
--
-- Sem o sweep (expiração 100% via schedule asynq), holder e waitlister do mesmo
-- produto ganham o MESMO expires_at no finalize → duas tasks cart.expire disparam
-- concorrentes. Dois guards adicionais fecham essa corrida:
--   (a) expires_at < now(): um cart com janela ESTENDIDA no futuro (promovido da
--       fila) não pode ser expirado por uma task com snapshot velho — o WHERE
--       relê o valor commitado, então a extensão vence a task antiga (MVCC).
--   (b) NOT EXISTS(...): o ciclo de vida de um cart de waitlister é governado
--       PELA FILA, não pelo próprio timer. Abstém-se enquanto o item está
--       'waiting' (na fila) OU 'notified' dentro da janela de promoção ainda
--       vigente (wi.expires_at > now(), gravada ATOMICAMENTE no claim). Isso
--       cobre a sub-janela entre o claim (waiting→notified) e o lock do promotor:
--       no instante em que o item vira 'notified' sua janela já é futura, então
--       a task do próprio waitlister se abstém — nunca deixa um cart
--       notified+expired segurando estoque vazado. Um 'notified' com janela já
--       VENCIDA (não pagou no prazo estendido) volta a ser elegível → expira.
-- 0 rows → não-elegível; o caller (ExpireCartAndReleaseStock) trata como skip.
UPDATE carts
SET status = 'expired', cancelled_reason = 'expired'
WHERE carts.id = $1
  AND status IN ('active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
  AND expires_at < now()
  AND NOT EXISTS (
      SELECT 1 FROM waitlist_items wi
      WHERE wi.cart_id = carts.id
        AND (wi.status = 'waiting'
             OR (wi.status = 'notified' AND wi.expires_at > now()))
  )
RETURNING *;

-- name: CancelCart :one
-- Cancelamento MANUAL pelo lojista (LIV-84). Mesmo desenho guard-first do
-- ExpireCart: o guard vive DENTRO do UPDATE para fechar a corrida com o webhook
-- de pagamento — se o cliente pagou no intervalo, 0 rows retornam e o caller
-- ABORTA sem devolver estoque nem cancelar nada no ERP (o pagamento vence).
--
-- Diferenças propositais em relação ao ExpireCart:
--   • sem guard de expires_at: a intenção do lojista não espera prazo nenhum;
--   • sem guard de waitlist: o cart morre por decisão humana, então os itens em
--     fila DESTE cart são cancelados junto (CancelWaitlistItemsByCart) em vez de
--     manterem o cart vivo.
-- 'expired'/'cancelled' não são reelegíveis → o flip é idempotente.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'store_cancelled'
WHERE carts.id = $1
  AND status IN ('active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;

-- name: RestoreCancelledCartAsPaid :one
-- O outro lado da corrida cancelamento × pagamento: o webhook confirmou o
-- pagamento DEPOIS que o lojista cancelou (PIX pago com o QR já aberto, cartão
-- em análise, webhook atrasado). Regra de negócio: **pagamento vence** — o
-- pedido volta e consta como PAGO, nunca como cancelado.
--
-- Restaura o cart para 'checkout' + pago e zera as colunas de ERP para que a
-- finalização pós-pagamento crie um pedido de venda LIMPO no Tiny (o
-- cancelamento já cancelou/estornou o anterior). O estoque local retomado fica
-- por conta do caller, na MESMA transação.
-- Guard: só restaura cart cancelado PELO LOJISTA e ainda não pago — um cart
-- expirado, bloqueado por handle ou já pago não entra por aqui.
UPDATE carts
SET status              = 'checkout',
    cancelled_reason    = NULL,
    payment_status      = $2,
    checkout_id         = $3,
    paid_at             = $4,
    payment_method      = $5,
    expires_at          = NULL,
    -- Carimbo do caso para o histórico do pedido e para o aviso no sino do
    -- painel: sem ele o lojista descobriria por acidente que vendeu algo que
    -- julgava cancelado.
    cancellation_reverted_at = now(),
    erp_order_state     = 'none',
    external_order_id   = NULL,
    erp_stock_launched  = FALSE,
    erp_op_started_at   = NULL
WHERE id = $1
  AND status = 'cancelled'
  AND cancelled_reason = 'store_cancelled'
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;

-- name: UpdateCartPayment :one
-- $3 = payment-provider ID (MP/Pagar.me). Goes to checkout_id, not
-- external_order_id — the latter is reserved for the ERP (Tiny) order ID
-- and is the idempotency key for finalizeCartERPOrder.
-- Guard (must-fix da corrida com o worker de expiração): recusa marcar
-- pagamento em cart já 'expired'/'cancelled' — 0 rows sinalizam ao caller que
-- não deve finalizar (evita o estado inconsistente pago+expirado e o cancel de
-- pedido ERP de venda paga). Ao pagar, neutraliza expires_at para que o worker
-- nunca reselcione a venda.
UPDATE carts
SET payment_status = $2, checkout_id = $3, paid_at = $4, payment_method = $5,
    expires_at = CASE WHEN $2 = 'paid' THEN NULL ELSE expires_at END
WHERE id = $1
  AND status NOT IN ('expired', 'cancelled')
RETURNING *;

-- name: UpdateCartNotifyStatus :one
UPDATE carts
SET notify_status = $2, notify_error = $3, notified_at = $4
WHERE id = $1
RETURNING *;

-- name: ListCartsByEvent :many
SELECT * FROM carts WHERE event_id = $1 ORDER BY created_at;

-- name: CreateCartItem :one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpsertCartItem :one
-- Adds quantity to existing cart item or creates new one
-- waitlisted_quantity is added to existing (not replaced)
-- session_id is first-touch: kept from the original add (COALESCE), so
-- re-adds in later sessions accumulate quantity under the first session.
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cart_id, product_id)
DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity,
             waitlisted_quantity = cart_items.waitlisted_quantity + EXCLUDED.waitlisted_quantity,
             session_id = COALESCE(cart_items.session_id, EXCLUDED.session_id)
RETURNING *;

-- name: InsertCartItemEvent :exec
-- Registra uma ADIÇÃO no log de atribuição (RN-12). Vai no mesmo tx do
-- UpsertCartItem: se um falhar, nenhum grava, e o invariante
-- SUM(log.quantity) >= cart_items.quantity nunca é violado por gravação
-- parcial. Só adições entram — remoção é tratada na alocação do selamento.
INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
VALUES ($1, $2, $3, $4, $5);

-- name: ListCartItemEventsForCart :many
-- O log de adições de um carrinho, em ordem cronológica. É o que o selamento
-- percorre para repartir a quantidade final entre as sessões (RN-29). O id
-- desempata adições gravadas no mesmo instante, tornando a ordem determinística.
SELECT product_id, session_id, quantity, unit_price, created_at
FROM cart_item_events
WHERE cart_id = $1
ORDER BY product_id, created_at, id;

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- SessionAttributionByEvent foi REMOVIDA (Fatia 5). Nunca teve chamador Go — e
-- não era aproveitável, por três motivos que a métrica em dois níveis proíbe:
--   1. chaveava por cart_items.session_id, que é FIRST-TOUCH (o COALESCE do
--      UpsertCartItem): creditava à segunda o que foi vendido na quarta;
--   2. subtraía waitlisted_quantity, enquanto a receita do evento
--      (cart_product_total_cents / orders.total_cents) usa quantity CHEIO — a
--      soma das sessões daria SEMPRE menos que o total do evento;
--   3. recalculava dos cart_items vivos filtrando payment_status, em vez de ler
--      o congelado (RN-14).
-- Quem responde "quanto a transmissão de terça faturou" agora é o par
-- ListSessionConfirmedRevenueByEvent (confirmado, de order_items) +
-- ListOpenCartItemsByEvent/ListCartItemEventsByEvent (projetado, alocado sobre
-- o log pelo MESMO AllocateBySession do selamento).

-- name: FinalizeCartsByEvent :many
-- RN-06: ao encerrar o EVENTO, o carrinho sai de 'active' ("pode pagar, sem
-- prazo") para 'checkout' ("prazo correndo") e ganha expires_at. Encerrar uma
-- SESSÃO não passa por aqui — sessão não mexe em carrinho.
--
-- O carrinho PAGO não transiciona (A10). Antes, o filtro de payment_status
-- incidia só no CASE do expires_at, nunca no WHERE: o carrinho pago virava
-- 'checkout' — que no vocabulário novo significa "prazo correndo" — e ainda
-- gerava um cart.checkout_armed inútil, que virava um ScheduleExpiry no-op.
-- Com a decisão 7 (pagar durante o evento), isso deixou de ser detalhe.
--
-- O prazo NÃO é resolvido aqui: chega pronto em $2, vindo de
-- GetEventCartSettings — a fonte única que já aplica a RN-34 (curto x
-- estendido, conforme close_cart_on_event_end) e o fallback para a loja. O
-- COALESCE inline que existia aqui era a terceira cópia da mesma regra.
--
-- QUEM ESTÁ NA FILA GANHA O PRAZO EXTRA DO EVENTO.
--
-- Todo carrinho recebia o MESMO expires_at — um UPDATE, um now() — então os
-- três vencidos em 04/08 tinham 18:38:51.703178 idêntico ao microssegundo. Quem
-- esperava um produto morria no mesmo instante de quem o segurava, e a promoção
-- da fila não tinha intervalo nenhum para acontecer: as três linhas terminaram
-- 'expired' com notified_at NULL. Ninguém foi avisado.
--
-- O extra é `waitlist_notified_ttl_minutes` do EVENTO — a mesma configuração
-- que o lojista já preenche para responder "quanto tempo A MAIS quem espera
-- tem". Não é número novo nem regra nova: é a regra dele, aplicada onde
-- finalmente importa. Sem isso ela só valia depois da promoção, e a promoção
-- nunca chegava.
--
-- O critério é ter item AGUARDANDO ou PROMOVIDO com janela viva. Item já
-- 'expired' ou 'fulfilled' não estende nada — quem não espera mais não precisa
-- de prazo maior.
--
-- Retorna os ids finalizados para emitir cart.checkout_armed por carrinho.
UPDATE carts c
SET status = 'checkout',
    expires_at = now() + make_interval(mins =>
        sqlc.arg(expiration_minutes)::int
        + CASE WHEN EXISTS (
              SELECT 1 FROM waitlist_items wi
              WHERE wi.cart_id = c.id
                AND (wi.status = 'waiting'
                     OR (wi.status = 'notified' AND wi.expires_at > now()))
          ) THEN sqlc.arg(waitlist_extra_minutes)::int ELSE 0 END)
WHERE c.event_id = $1
  AND c.status = 'active'
  AND c.payment_status IS DISTINCT FROM 'paid'
RETURNING c.id;

-- name: ShiftOpenCartExpirations :many
-- Propagação da edição de prazo do evento para quem JÁ está com o relógio
-- correndo: desloca expires_at pelo delta entre o prazo efetivo novo e o
-- antigo. DESLOCA, não recalcula — recalcular do zero apagaria as extensões
-- individuais (prazo extra da fila no finalize, GREATEST do reopen RN-10).
--
-- Quem fica de fora, e por quê:
--   • expires_at IS NULL — RN-04: evento ativo não tem relógio; o valor novo
--     passa a valer sozinho no fechamento (FinalizeCartsByEvent lê da fonte
--     única GetEventCartSettings);
--   • pago — A10: pagamento neutraliza o prazo, nada a deslocar;
--   • terminal (expired/cancelled) — o desfecho já aconteceu; "reviver" um
--     carrinho expirado por edição de configuração seria decisão de negócio
--     nova, não propagação.
-- O deslocamento pode cair no passado (lojista ENCURTOU dias depois): correto —
-- o cart.expire re-armado dispara na hora e o guard decide, como sempre.
UPDATE carts
SET expires_at = expires_at + make_interval(mins => sqlc.arg(delta_minutes)::int)
WHERE event_id = $1
  AND status IN ('active', 'checkout')
  AND payment_status IS DISTINCT FROM 'paid'
  AND expires_at IS NOT NULL
RETURNING id;

-- name: CountCartsByEvent :one
SELECT COUNT(*)::int as count FROM carts WHERE event_id = $1 AND status = 'active';

-- name: UpdateCartItem :one
UPDATE cart_items
SET quantity = $2
WHERE id = $1
RETURNING *;

-- name: DeleteCartItem :exec
DELETE FROM cart_items WHERE id = $1;

-- name: GetCartItem :one
SELECT * FROM cart_items WHERE id = $1;

-- =============================================================================
-- EVENT DETAILS - Stats and Cart Listing
-- =============================================================================

-- name: GetEventStats :one
-- Returns stats for an event: comments, carts, revenue, products sold, funnel metrics
--
-- ⚠️ O PREDICADO DO CARRINHO ABERTO ("ainda em aberto") aparece aqui em
-- open_carts e projected_revenue e é repetido, ao pé da letra, em
-- ListOpenCartItemsByEvent e ListCartItemEventsByEvent. É esse par que faz a
-- soma das transmissões fechar com o total do evento; mudar um lado só quebra
-- o invariante da Fatia 5 em silêncio.
--
-- Ele exclui o carrinho JÁ PAGO. O carrinho não muda de status ao ser pago
-- (UpdateCartPayment mexe só em payment_status), então ele fica em 'checkout'
-- para sempre — e sem esta exclusão o mesmo dinheiro era contado duas vezes na
-- mesma tela: uma em confirmed_revenue (a venda) e outra em projected_revenue
-- ("expectativa"). A tooltip do painel promete o contrário ("carrinhos abertos
-- que ainda não foram pagos"). 'refunded' entra na exclusão pelo mesmo motivo:
-- estorno não devolve o carrinho para a expectativa de venda.
-- COALESCE porque payment_status é NULLable desde a 000001 e `NULL NOT IN (…)`
-- é NULL — sem ele o carrinho antigo sumiria do projetado.
SELECT
    -- Funnel metrics
    COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = $1), 0)::int AS total_comments,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1), 0)::int AS total_carts,
    COALESCE((
        SELECT COUNT(*) FROM carts ct
        WHERE ct.event_id = $1
          AND ct.status IN ('active', 'checkout')
          AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
    ), 0)::int AS open_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND (ct.status = 'checkout' OR ct.checkout_url IS NOT NULL)), 0)::int AS checkout_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Product metrics — "unidades vendidas" sai de order_items de pedido PAGO,
    -- a MESMA fonte de ListProductsByEvent (Top Produtos) e de
    -- ListSessionConfirmedRevenueByEvent (a quebra por transmissão). Antes era
    -- SUM(cart_items.quantity) de todo carrinho não expirado, ou seja: carrinho
    -- aberto, pendente, cancelado e até a fila de espera contavam como venda —
    -- três definições de "vendido" na mesma tela, e a tooltip prometia a mais
    -- restrita ("unidades efetivamente vendidas").
    COALESCE((
        SELECT SUM(oi.quantity)
        FROM order_items oi
        JOIN orders o ON o.id = oi.order_id
        WHERE o.event_id = $1 AND o.status = 'paid'
    ), 0)::int AS total_products_sold,
    -- Revenue metrics (Grupo C: projected stays cart-based; Grupo A: confirmed reads from sealed orders)
    COALESCE((
        SELECT SUM(cart_product_total_cents(ct.id))
        FROM carts ct
        WHERE ct.event_id = $1
          AND ct.status IN ('active', 'checkout')
          AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
    ), 0)::bigint AS projected_revenue,
    COALESCE((
        SELECT SUM(o.total_cents)
        FROM orders o
        WHERE o.event_id = $1 AND o.status = 'paid'
    ), 0)::bigint AS confirmed_revenue;

-- name: ListCartsWithTotalByEvent :many
-- Returns carts for an event with total value and item count (available vs waitlisted)
SELECT
    c.id,
    c.event_id,
    c.session_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.created_at,
    c.expires_at,
    COALESCE(SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price), 0)::bigint AS total_value,
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    COALESCE(SUM(ci.quantity - ci.waitlisted_quantity), 0)::int AS available_items,
    COALESCE(SUM(ci.waitlisted_quantity), 0)::int AS waitlisted_items
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC;

-- name: ListProductsByEvent :many
-- Returns products sold in an event with quantity and revenue (paid orders only)
SELECT
    p.id,
    p.name,
    p.image_url,
    p.keyword,
    COALESCE(SUM(oi.quantity), 0)::int AS total_quantity,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::bigint AS total_revenue
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
JOIN products p ON p.id = oi.product_id
WHERE o.event_id = $1 AND o.status = 'paid'
GROUP BY p.id, p.name, p.image_url, p.keyword
ORDER BY total_quantity DESC;

-- =============================================================================
-- MÉTRICA EM DOIS NÍVEIS — por SESSÃO e agregada por EVENTO (Fatia 5)
--
-- GetSessionStats foi REMOVIDA. Ela agrupava por carts.session_id, que é a
-- sessão em que o carrinho NASCEU (gravada uma única vez no CreateCart e nunca
-- mais tocada). Com o carrinho unificado da campanha (RN-02) um carrinho vive a
-- semana inteira, então o carrinho INTEIRO — inclusive o que foi comprado na
-- quinta — era creditado à transmissão de segunda. Além disso ela era chamada
-- uma vez POR SESSÃO dentro do laço de GetEventWithSessions (N+1) para alimentar
-- campos que a tela sequer renderizava.
--
-- No lugar entram três queries de EVENTO (uma chamada, não N):
--   • confirmado  ← order_items.session_id, o congelado do pedido pago;
--   • projetado   ← cart_items (quantidade final) + cart_item_events (de onde
--                   veio cada unidade), repartido em Go por AllocateBySession —
--                   a MESMA função que o selamento usa. Duas implementações da
--                   mesma regra seria exatamente a divergência que a métrica em
--                   dois níveis não pode ter.
--
-- O INVARIANTE (inegociável): a soma das sessões bate exatamente com o total do
-- evento em GetEventStats. Por isso cada query abaixo repete, ao pé da letra, o
-- predicado do seu par lá em cima — e por isso o balde session_id IS NULL ("sem
-- transmissão") é devolvido junto: sem ele a soma não fecha.
-- =============================================================================

-- name: ListSessionConfirmedRevenueByEvent :many
-- Receita CONFIRMADA por transmissão: o congelado do pedido, repartido pela
-- sessão que vendeu cada unidade (RN-13). Uma linha por sessão + a linha
-- session_id NULL (adição sem transmissão, ou pedido anterior ao log).
--
-- Fecha com GetEventStats.confirmed_revenue (SUM(orders.total_cents)) por
-- construção: o selamento grava order_items com o unit_price do carrinho e
-- AllocateBySession garante SUM(order_items.quantity) == cart_items.quantity,
-- logo SUM(quantity*unit_price) == cart_product_total_cents == total_cents.
SELECT
    oi.session_id,
    COALESCE(SUM(oi.quantity), 0)::int                    AS sold_units,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::bigint AS revenue_cents,
    COUNT(DISTINCT o.cart_id)::int                        AS paid_carts
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
WHERE o.event_id = $1 AND o.status = 'paid'
GROUP BY oi.session_id;

-- name: ListOpenCartItemsByEvent :many
-- A quantidade FINAL de cada produto nos carrinhos que entram na projeção.
--
-- O predicado e a quantidade CHEIA são cópia literal de
-- GetEventStats.projected_revenue — inclusive a exclusão do carrinho já pago
-- ou estornado, que entrou junto nos dois lados. O que a Fatia 5 garante é que
-- os dois níveis usem O MESMO predicado; qualquer mudança aqui tem de acontecer
-- lá em cima na mesma edição, senão a soma das sessões deixa de fechar.
SELECT
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price
FROM carts ct
JOIN cart_items ci ON ci.cart_id = ct.id
WHERE ct.event_id = $1
  AND ct.status IN ('active', 'checkout')
  AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
ORDER BY ci.cart_id, ci.product_id;

-- name: ListCartItemEventsByEvent :many
-- O log de adições dos MESMOS carrinhos da query acima, na ordem que
-- AllocateBySession exige (cronológica por produto, id desempatando).
-- Ele diz de onde veio cada unidade; a quantidade final continua sendo a de
-- cart_items.
SELECT
    cie.cart_id,
    cie.product_id,
    cie.session_id,
    cie.quantity,
    cie.unit_price
FROM cart_item_events cie
JOIN carts ct ON ct.id = cie.cart_id
WHERE ct.event_id = $1
  AND ct.status IN ('active', 'checkout')
  AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
ORDER BY cie.cart_id, cie.product_id, cie.created_at, cie.id;

-- =============================================================================
-- ERP SYNC & EXPIRATION
-- =============================================================================

-- name: UpdateCartExternalOrderID :exec
UPDATE carts SET external_order_id = $2 WHERE id = $1;

-- Fatia 10-b: as queries pós-venda de finalização/NF do ERP saíram daqui. A
-- Fatia 11b tornou order_payments a fonte autoritativa e repontou os wrappers Go
-- (MarkCartERPFinalisation*/GetCartERPFinalisationStatus/Get|UpsertCartERPInvoice
-- em repository.go) para as queries Order* em order_write.sql. As colunas
-- pós-venda correspondentes foram dropadas do cart (migration 000101). O que fica
-- no cart é só reserva (erp_order_state/erp_stock_launched/erp_op_started_at) e
-- external_order_id.

-- name: FindCartByExternalOrderID :one
-- Resolve a cart from a Tiny pedido id (or any ERP external order id) for a
-- given store. Used by the Tiny webhook handler when only the pedido id is
-- available on the payload.
SELECT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
WHERE c.external_order_id = $1
  AND le.store_id = $2
ORDER BY c.created_at DESC
LIMIT 1;

-- name: ListNonWaitlistedCartItems :many
-- Returns cart items that have available (non-waitlisted) quantity, with product external_id for ERP sync
-- Returns available_quantity = quantity - waitlisted_quantity
SELECT ci.id, ci.cart_id, ci.product_id,
       (ci.quantity - ci.waitlisted_quantity) AS quantity,
       ci.unit_price, ci.waitlisted_quantity,
       p.name AS product_name, p.external_id AS product_external_id,
       p.keyword AS product_keyword, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1 AND ci.quantity > ci.waitlisted_quantity;

-- name: ListExpiredCartsByEventAndProduct :many
-- Returns expired carts for a specific event that contain a specific product (with available qty)
SELECT DISTINCT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
  AND c.status = 'active'
  AND c.expires_at IS NOT NULL
  AND c.expires_at < now()
  AND ci.product_id = $2
  AND ci.quantity > ci.waitlisted_quantity;

-- name: DeleteCartItemByCartAndProduct :exec
DELETE FROM cart_items WHERE cart_id = $1 AND product_id = $2;

-- name: DeleteCartItemsByCart :exec
-- Apaga todos os itens de um cart. Usado ao reabrir um cart expirado/cancelado
-- para reuso (os itens antigos já tiveram o estoque devolvido pela expiração).
DELETE FROM cart_items WHERE cart_id = $1;

-- name: DecrementCartItemQuantity :one
-- Subtrai @delta da quantidade do item; se zerar, retorna a row mas a
-- limpeza fica a cargo do caller (delete em outro statement). Não permite
-- ir negativo.
UPDATE cart_items
SET quantity = GREATEST(quantity - @delta::int, 0)
WHERE cart_id = $1 AND product_id = $2
RETURNING quantity;

-- name: UpdateCartItemWaitlistedQuantity :exec
UPDATE cart_items SET waitlisted_quantity = $3 WHERE cart_id = $1 AND product_id = $2;

-- name: GetCartByEventAndUserForUpdate :one
-- O carrinho ABERTO, travado para escrita. Esta é a do caminho quente
-- (GetOrCreateCart). Mesmo filtro do GetCartByEventAndUser: sem ele, o FOR
-- UPDATE poderia travar a linha errada — por exemplo a de um carrinho já pago.
SELECT * FROM carts
WHERE event_id = $1 AND platform_user_id = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at DESC
LIMIT 1
FOR UPDATE;

-- name: GetProductQuantityInUserCart :one
-- Quantidade de um produto no carrinho ABERTO do comprador neste evento.
--
-- Alimenta o teto cart_max_quantity_per_item. Sem o filtro, com mais de um
-- carrinho a leitura vira não-determinística e o teto passa a valer sobre um
-- carrinho arbitrário — inclusive um já pago, o que bloquearia compra legítima.
SELECT COALESCE(ci.quantity, 0)::INT AS quantity
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id AND ci.product_id = $3
WHERE c.event_id = $1 AND c.platform_user_id = $2
  AND c.status IN ('pending', 'active', 'checkout')
  AND (c.payment_status IS NULL OR c.payment_status NOT IN ('paid', 'refunded'))
ORDER BY c.created_at DESC
LIMIT 1;

-- =============================================================================
-- PUBLIC CHECKOUT - Cart page for customers
-- =============================================================================

-- name: GetCartByTokenWithDetails :one
-- Returns cart with event info for public checkout page
SELECT
    c.id,
    c.event_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.checkout_url,
    c.checkout_id,
    c.checkout_expires_at,
    c.customer_email,
    c.payment_status,
    c.paid_at,
    c.payment_integration_id,
    c.created_at,
    c.expires_at,
    c.initial_snapshot_taken_at,
    c.initial_subtotal_cents,
    c.coupon_id,
    c.coupon_code,
    c.coupon_discount_cents,
    c.cancelled_reason,
    le.title AS event_title,
    le.store_id,
    s.slug AS store_slug,
    s.name AS store_name,
    s.logo_url AS store_logo_url,
    s.cart_allow_edit AS allow_edit,
    s.cart_max_quantity_per_item AS max_quantity_per_item,
    -- Parcela minima da loja: a regra de quantas parcelas cabem no total mora no
    -- servidor (checkout.MaxInstallmentsFor), e ela precisa deste numero tanto
    -- para montar a lista que o comprador ve quanto para recusar um POST que
    -- peca mais parcelas do que a loja permite.
    s.min_installment_cents AS min_installment_cents
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN stores s ON s.id = le.store_id
WHERE c.token = $1;

-- name: BindCartPaymentIntegration :exec
-- Pins the cart to a specific payment integration the moment GetCheckoutConfig
-- successfully instantiates a provider. Subsequent ProcessCardPayment /
-- GeneratePixPayment calls use this binding instead of re-resolving the
-- store's primary, so a FE that tokenized with provider X always reaches
-- provider X server-side (card tokens are provider-bound).
UPDATE carts
SET payment_integration_id = $2
WHERE id = $1;

-- name: ListCartItemsForCheckout :many
-- Returns cart items with product details for checkout page.
-- product_stock is exposed so the public checkout can disable the "+" button
-- when the buyer is about to exceed the available stock for the SKU.
SELECT
    ci.id,
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price,
    ci.waitlisted_quantity,
    p.name AS product_name,
    p.image_url AS product_image_url,
    p.keyword AS product_keyword,
    p.stock AS product_stock
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1
ORDER BY ci.id;

-- name: UpdateCartCustomerEmail :one
UPDATE carts
SET customer_email = $2
WHERE token = $1
RETURNING *;

-- name: UpdateCartCustomerCheckout :one
-- Persists full customer + shipping data entered in the checkout form.
-- Called right before a card/pix payment request so the webhook handler
-- (and ERP order creation on paid) has complete info regardless of provider.
UPDATE carts
SET customer_email    = $2,
    customer_name     = $3,
    customer_document = $4,
    customer_phone    = $5,
    shipping_address  = $6,
    whatsapp_consent  = $7,
    whatsapp_consent_at = CASE
      WHEN $7 = TRUE AND whatsapp_consent = FALSE THEN NOW()
      ELSE whatsapp_consent_at
    END
WHERE id = $1
RETURNING *;

-- name: UpdateCartCheckoutInfo :one
-- Updates checkout URL and ID after generating payment link
UPDATE carts
SET checkout_url = $2, checkout_id = $3, checkout_expires_at = $4
WHERE id = $1
RETURNING *;

-- name: GetCartByCheckoutID :one
-- Used by webhook to find cart when payment is confirmed
SELECT * FROM carts WHERE checkout_id = $1;

-- name: UpdateCartPaymentByCheckoutID :one
-- Updates payment status when webhook confirms payment
UPDATE carts
SET payment_status = $2, paid_at = $3
WHERE checkout_id = $1
RETURNING *;

-- name: UpdateCartPaymentStatus :one
-- Updates payment status directly by cart ID (for transparent checkout)
-- Uses checkout_id to store the payment ID from the provider.
-- Mesmo guard de UpdateCartPayment: não marca pagamento em cart expirado/
-- cancelado (0 rows = caller não finaliza) e neutraliza expires_at ao pagar.
UPDATE carts
SET payment_status = $2, checkout_id = $3, paid_at = $4,
    expires_at = CASE WHEN $2 = 'paid' THEN NULL ELSE expires_at END
WHERE id = $1
  AND status NOT IN ('expired', 'cancelled')
RETURNING *;

-- name: GetStorePaymentIntegration :one
-- Returns the highest-priority active payment integration for a store.
-- Lower `priority` wins; created_at tie-breaks (oldest first within a tie).
-- See ListStorePaymentIntegrations for the fallback chain.
SELECT i.*
FROM integrations i
WHERE i.store_id = $1
  AND i.type = 'payment'
  AND i.status = 'active'
ORDER BY i.priority ASC, i.created_at ASC
LIMIT 1;

-- name: ListStorePaymentIntegrations :many
-- Returns all active payment integrations for a store in priority order.
-- The checkout layer walks this list as a fallback chain: if the primary
-- can't be instantiated (credentials corrupted, provider unsupported, etc.),
-- the next one is tried.
SELECT i.*
FROM integrations i
WHERE i.store_id = $1
  AND i.type = 'payment'
  AND i.status = 'active'
ORDER BY i.priority ASC, i.created_at ASC;

-- name: HasInFlightFinalisationForProduct :one
-- Defesa em profundidade do backstop de waitlist: TRUE se existe cart pago
-- com finalização ERP em andamento (ou falha recente, ainda retomável via
-- retry) contendo o produto. Enquanto verdadeiro, promoção disparada por
-- webhook de estoque é adiada — a DM de promoção é irreversível.
-- Fatia 11b: o estado de finalização é autoritativo em order_payments (join via
-- Order). COALESCE('pending') cobre o cart pago cuja Order ainda materializa. As
-- colunas de reserva (erp_order_state/erp_op_started_at) seguem no cart.
SELECT EXISTS(
    SELECT 1 FROM carts c
    JOIN cart_items ci ON ci.cart_id = c.id
    LEFT JOIN orders o          ON o.cart_id  = c.id
    LEFT JOIN order_payments op ON op.order_id = o.id
    WHERE ci.product_id = $1
      AND ((c.payment_status = 'paid'
            AND COALESCE(op.erp_finalisation_status, 'pending') <> 'done'
            AND (c.paid_at > now() - interval '30 minutes'
                 OR op.erp_last_attempt_at > now() - interval '30 minutes'))
           -- design C: ciclo de conversão/mutação em voo — adia a promoção
           OR (c.erp_order_state IN ('converting','mutating')
               AND c.erp_op_started_at > now() - interval '30 minutes'))
)::bool AS in_flight;

-- name: GetCartItemAvailableQty :one
-- Unidades não-waitlisted de um item (o que foi decrementado do estoque
-- local no add e deve voltar quando o cart expira).
SELECT (quantity - waitlisted_quantity)::int AS available
FROM cart_items
WHERE cart_id = $1 AND product_id = $2;

-- Fatia 10-b: MarkCartERPFinalisationAttempt (S1 da finalização retomável) saiu
-- daqui — o wrapper Go repontou para MarkOrderERPFinalisationAttempt
-- (order_write.sql) na Fatia 11b e as colunas erp_payment_snapshot/
-- erp_last_attempt_at foram dropadas do cart (migration 000101).

-- ============================================================================
-- DESIGN C — pedido Tiny como reserva a partir da iniciação do pagamento
-- ============================================================================

-- name: TransitionCartERPOrderState :execrows
-- CAS de transição da máquina de estados do pedido-como-reserva. rows=0 quando
-- o estado atual não é o esperado — single-flight de conversão/mutação.
UPDATE carts
SET erp_order_state = sqlc.arg(to_state)::varchar,
    erp_op_started_at = CASE WHEN sqlc.arg(to_state)::varchar IN ('converting','mutating') THEN now() ELSE erp_op_started_at END
WHERE id = sqlc.arg(cart_id) AND erp_order_state = sqlc.arg(from_state);

-- name: GetCartERPOrderState :one
SELECT erp_order_state, erp_stock_launched, COALESCE(external_order_id,'') AS external_order_id
FROM carts WHERE id = $1;

-- name: SetCartERPStockLaunched :exec
UPDATE carts SET erp_stock_launched = $2 WHERE id = $1;

-- name: ListStuckERPOrderOps :many
-- Sweep: conversões/mutações em voo há mais tempo que o limiar — o processo
-- morreu no meio; o sweep reconcilia (adota pedido via marcador ou re-roda o
-- ciclo). NUNCA resetar para 'none' (a chamada em voo pode ter sucedido
-- server-side e o caminho legado criaria pedido duplicado).
SELECT c.id, c.erp_order_state, c.erp_op_started_at, COALESCE(c.external_order_id,'') AS external_order_id,
       le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
WHERE c.erp_order_state IN ('converting','mutating')
  AND c.erp_op_started_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::int);

-- name: GetCartGMVCents :one
-- GMV de um cart: delega à função canônica cart_product_total_cents (migration 000093).
SELECT cart_product_total_cents(sqlc.arg(cart_id))::bigint AS gmv_cents;

-- name: SetCartItemSplitIfUnchanged :execrows
-- Escreve a quantidade TOTAL e a parte em FILA de uma vez, e só se ninguém
-- tiver mexido na linha desde que a lemos.
--
-- Duas correções numa query, porque as duas vivem no mesmo UPDATE:
--
-- 1) TRAVA OTIMISTA. O UpdateCartItem antigo era `SET quantity = $2 WHERE
--    id = $1` — absoluto e sem guarda. Entre a leitura e a escrita, o PATCH do
--    checkout faz uma chamada HTTP ao Tiny que passa pelo limitador de ~1 req/s:
--    a janela dura SEGUNDOS. Em 05/08 dois escritores leram quantity=2 com
--    3.070 ms de diferença; o segundo calculou o delta contra um valor já
--    obsoleto, debitou 2 e a linha só andou 1. Uma unidade sumiu do LiveCart e
--    uma saída a mais foi lançada no Tiny. `AND quantity = @expected_quantity`
--    faz o perdedor afetar ZERO linhas, e o chamador devolve conflito em vez de
--    corromper a conta.
--
-- 2) A PARTE EM FILA ACOMPANHA. `waitlisted_quantity` é a parcela SEM estoque;
--    o que está segurado é `quantity - waitlisted_quantity`. O update antigo
--    mexia só no total, então baixar de 5 (com 3 em fila, 2 segurados) para 2
--    creditava 3 unidades ao estoque quando só 2 haviam sido tiradas — e ainda
--    deixava a linha com disponível NEGATIVO (2 - 3). Escrever as duas colunas
--    juntas é o que mantém a identidade `total = segurado + em fila`.
UPDATE cart_items
SET quantity = @quantity::int,
    waitlisted_quantity = @waitlisted_quantity::int
WHERE id = @id
  AND quantity = @expected_quantity::int;

-- name: FindOpenCartUserIDByHandle :one
-- O platform_user_id do carrinho ABERTO deste comprador no evento, procurado
-- pelo @ em vez do id.
--
-- Existe porque o MESMO comprador chega com DOIS ids diferentes conforme o
-- caminho: o webhook traz `from.self_ig_scoped_id` (o IGSID, único aceito pela
-- API de mensagens como destinatário) e a aresta `/{media}/comments` do polling
-- não devolve esse campo — só o `from.id` cru.
--
-- Em 05/08 isso deu DOIS carrinhos para @englivecart no mesmo evento: um com
-- 1498886768484002 (webhook) e outro com 17841439350112281 (polling). A unique
-- parcial por (evento, platform_user_id) não viu violação nenhuma — os ids são
-- diferentes de verdade. O índice fez o trabalho dele; a entrada é que chegou
-- com duas identidades para a mesma pessoa.
--
-- Só bate em quem já tem carrinho aberto no evento: o @ é estável dentro de uma
-- transmissão e é por ele que o lojista reconhece o comprador.
SELECT platform_user_id FROM carts
WHERE event_id = $1
  AND platform_handle = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at ASC
LIMIT 1;

-- name: SetCartPixCharge :exec
-- Guarda a cobranca PIX viva do carrinho: o id que da para cancelar e o valor
-- que o QR na mao do comprador cobra.
UPDATE carts
SET pix_charge_id = sqlc.arg(pix_charge_id),
    pix_amount_cents = sqlc.arg(pix_amount_cents)
WHERE id = sqlc.arg(id);

-- name: ClearCartPixCharge :exec
-- Esquece a cobranca PIX. Chamado depois de cancelar no gateway; separado da
-- escrita para o cancelamento poder falhar sem deixar um id fantasma aqui.
UPDATE carts
SET pix_charge_id = NULL,
    pix_amount_cents = NULL
WHERE id = sqlc.arg(id);

-- name: TakeCartPixCharge :one
-- Le e LIMPA a cobranca viva na mesma instrucao.
--
-- Duas mutacoes simultaneas no mesmo carrinho tentariam cancelar a mesma
-- cobranca duas vezes; quem le aqui e o unico que recebe o id, entao o
-- cancelamento no gateway sai uma vez so.
--
-- O valor devolvido tem de ser o ANTIGO. `RETURNING` num UPDATE entrega a linha
-- DEPOIS da escrita, entao a forma direta devolveria os NULL que acabamos de
-- gravar. A CTE le antes, e o `FOR UPDATE` nela serializa os concorrentes: o
-- segundo a chegar espera, reavalia o predicado com a coluna ja nula, nao
-- encontra linha e sai sem id — que e exatamente o vencedor unico que se quer.
--
-- Evita de proposito o `RETURNING OLD.*` do Postgres 18: a query passaria a
-- depender da versao do servidor, e o custo aqui e uma CTE.
WITH tomada AS (
    SELECT carts.id AS cart_id, carts.pix_charge_id, carts.pix_amount_cents
    FROM carts
    WHERE carts.id = sqlc.arg(id) AND carts.pix_charge_id IS NOT NULL
    FOR UPDATE
)
UPDATE carts c
SET pix_charge_id = NULL,
    pix_amount_cents = NULL
FROM tomada
WHERE c.id = tomada.cart_id
RETURNING tomada.pix_charge_id, tomada.pix_amount_cents;

-- name: CancelCartOnRefund :execrows
-- O reembolso mata a venda, e o carrinho precisa morrer junto.
--
-- Até 19/08 o estorno mexia em tudo (cobrança, cupom, e-mail, pedido no ERP)
-- MENOS no status do carrinho: ele ficava 'active' + payment 'refunded' para
-- sempre. Consequência dupla no painel: nunca aparecia em "Cancelados" (a aba
-- filtra por status do carrinho, de propósito) e ficava em "Precisam atenção"
-- eternamente, porque o matcher casa payment_status='refunded' e não existia
-- ação que o tirasse de lá — o cancelamento manual recusa carrinho não-pendente
-- ("pagamento vence"). Dois pedidos de teste reembolsados em 16/08 provaram o
-- beco: três dias parados na aba de triagem sem saída.
--
-- Guard-first e idempotente: só age sobre carrinho reembolsado ainda não
-- terminal. NÃO devolve estoque local nem toca fila — o fan-out do reembolso
-- (ReactOrderRefundedERP cancela o pedido no Tiny, que devolve o estoque lá; o
-- espelho traz o número de volta) é quem cuida disso.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'refunded'
WHERE id = $1
  AND payment_status = 'refunded'
  AND status NOT IN ('cancelled', 'expired');
