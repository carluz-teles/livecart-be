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
UPDATE carts
SET expires_at = GREATEST(
    COALESCE(expires_at, @new_expires_at::timestamptz),
    @new_expires_at::timestamptz
)
WHERE id = $1;

-- name: UpdateCartShippingAddress :exec
-- Replaces the cart's shipping_address JSONB. Used by the admin's "edit
-- address" action — narrower than UpdateCartCustomerCheckout (which also
-- writes customer fields) so we don't accidentally clear contact info.
UPDATE carts
SET shipping_address = $2
WHERE id = $1;

-- name: RegenerateCartCheckout :exec
-- Resets the checkout window for a cart so the buyer can pay again. Bumps
-- expires_at, brings status back to 'active' and payment_status to 'pending'
-- (covers expired/failed states), and clears any cached checkout url so the
-- next checkout-side call generates a fresh one.
UPDATE carts
SET expires_at         = $2,
    status             = 'active',
    payment_status     = 'pending',
    checkout_url       = NULL,
    checkout_id        = NULL,
    checkout_expires_at = NULL,
    -- Reset das colunas de RESERVA do ERP (must-fix C): reabrir um cart design-C
    -- sem isto deixaria erp_order_state/external_order_id obsoletos e o próximo
    -- pagamento cairia em "cart pago após cancelamento — reconciliação manual". A
    -- expiração já cancelou/estornou o pedido antigo; o reopen zera para um
    -- ciclo pagamento→ERP limpo. (As colunas pós-venda de finalização/NF vivem
    -- em order_payments desde a Fatia 11b — o cart reaberto é pré-pagamento e não
    -- carrega estado de finalização.)
    erp_order_state         = 'none',
    external_order_id       = NULL,
    erp_stock_launched      = FALSE,
    erp_op_started_at       = NULL
WHERE id = $1;

-- name: ReopenExpiredCartForReuse :exec
-- Reabre um cart 'expired'/'cancelled' para reuso pelo MESMO comprador no MESMO
-- evento (a unique (event_id, platform_user_id) impede criar um 2º cart). Reseta
-- TUDO para um estado limpo — inclusive as colunas de ERP: sem isso, um cart
-- design-C reaberto pagaria e cairia em "cart pago após cancelamento —
-- reconciliação manual" (external_order_id/erp_order_state obsoletos). Os
-- cart_items antigos são apagados à parte (DeleteCartItemsByCart) — já tiveram o
-- estoque devolvido pela expiração; o item novo entra fresco pelo fluxo de add.
UPDATE carts
SET status                  = 'active',
    payment_status          = 'pending',
    cancelled_reason        = NULL,
    expires_at              = NULL,
    checkout_url            = NULL,
    checkout_id             = NULL,
    checkout_expires_at     = NULL,
    paid_at                 = NULL,
    payment_method          = NULL,
    coupon_id               = NULL,
    coupon_code             = NULL,
    coupon_discount_cents   = 0,
    erp_order_state         = 'none',
    external_order_id       = NULL,
    erp_stock_launched      = FALSE,
    erp_op_started_at       = NULL
WHERE id = $1;

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
SELECT * FROM carts WHERE event_id = $1 AND platform_user_id = $2;

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

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- name: SessionAttributionByEvent :many
-- Per-session attribution within an event (first-touch): sold units and paid
-- revenue in cents, credited to the session each item was first added in.
-- Returns every session of the event, ordered, including zero-revenue ones.
SELECT
    s.id AS session_id,
    s.sequence_order,
    COALESCE(SUM(
        CASE WHEN c.payment_status = 'paid'
             THEN GREATEST(ci.quantity - ci.waitlisted_quantity, 0)
             ELSE 0 END), 0)::bigint AS sold_units,
    COALESCE(SUM(
        CASE WHEN c.payment_status = 'paid'
             THEN GREATEST(ci.quantity - ci.waitlisted_quantity, 0) * ci.unit_price
             ELSE 0 END), 0)::bigint AS revenue_cents
FROM live_sessions s
LEFT JOIN cart_items ci ON ci.session_id = s.id
LEFT JOIN carts c ON c.id = ci.cart_id
WHERE s.event_id = $1
GROUP BY s.id, s.sequence_order
ORDER BY s.sequence_order;

-- name: FinalizeCartsByEvent :many
-- Ao encerrar a live, move os carrinhos ativos para 'checkout' e arma o prazo
-- de pagamento: expires_at = now() + cart_expiration_minutes (do evento, com
-- fallback para o default da loja). 0 = sem expiração (preserva o que havia).
-- Carrinhos já pagos não recebem prazo — não faz sentido expirar uma venda.
-- Retorna os ids finalizados para emitir cart.checkout_armed por carrinho.
UPDATE carts c
SET status = 'checkout',
    expires_at = CASE
        WHEN c.payment_status <> 'paid'
             AND COALESCE(le.cart_expiration_minutes, s.cart_expiration_minutes, 0) > 0
        THEN now() + make_interval(mins => COALESCE(le.cart_expiration_minutes, s.cart_expiration_minutes))
        ELSE c.expires_at
    END
FROM live_events le
JOIN stores s ON s.id = le.store_id
WHERE c.event_id = $1 AND c.status = 'active' AND le.id = c.event_id
RETURNING c.id;

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
SELECT
    -- Funnel metrics
    COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = $1), 0)::int AS total_comments,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1), 0)::int AS total_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.status IN ('active', 'checkout')), 0)::int AS open_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND (ct.status = 'checkout' OR ct.checkout_url IS NOT NULL)), 0)::int AS checkout_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Product metrics
    COALESCE((
        SELECT SUM(ci.quantity)
        FROM carts ct
        JOIN cart_items ci ON ci.cart_id = ct.id
        WHERE ct.event_id = $1 AND ct.status != 'expired'
    ), 0)::int AS total_products_sold,
    -- Revenue metrics (Grupo C: projected stays cart-based; Grupo A: confirmed reads from sealed orders)
    COALESCE((
        SELECT SUM(cart_product_total_cents(ct.id))
        FROM carts ct
        WHERE ct.event_id = $1 AND ct.status IN ('active', 'checkout')
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
-- SESSION DETAILS - Stats per session
-- =============================================================================

-- name: GetSessionStats :one
-- Returns stats for a specific session: carts count and revenue
SELECT
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.session_id = $1), 0)::int AS total_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.session_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Grupo C: total_revenue stays cart-based (projection over all non-expired carts)
    COALESCE((
        SELECT SUM(cart_product_total_cents(ct.id))
        FROM carts ct
        WHERE ct.session_id = $1 AND ct.status != 'expired'
    ), 0)::bigint AS total_revenue,
    -- Grupo A: paid_revenue reads from sealed orders
    COALESCE((
        SELECT SUM(o.total_cents)
        FROM orders o
        JOIN carts ct ON ct.id = o.cart_id
        WHERE ct.session_id = $1 AND o.status = 'paid'
    ), 0)::bigint AS paid_revenue;

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
-- Lock the cart row for concurrent safety
SELECT * FROM carts WHERE event_id = $1 AND platform_user_id = $2 FOR UPDATE;

-- name: GetProductQuantityInUserCart :one
-- Returns the current quantity of a specific product in a user's cart for an event
SELECT COALESCE(ci.quantity, 0)::INT AS quantity
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id AND ci.product_id = $3
WHERE c.event_id = $1 AND c.platform_user_id = $2;

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
    s.cart_max_quantity_per_item AS max_quantity_per_item
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
