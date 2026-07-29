-- Backfill das Orders de vendas anteriores ao cutover das Fatias 7-11.
--
-- A materialização da Order (OnCartPaid) só passou a existir com o deploy de
-- 25-26/07/2026. Toda venda paga ANTES disso ficou sem linha em orders/
-- order_items/order_payments/order_logistics, e a tela de Pedidos — que lê do
-- agregado Order — simplesmente não mostra essas vendas. Em staging isso deixou
-- 6 pedidos pagos invisíveis; em produção o número é o histórico inteiro
-- anterior ao deploy.
--
-- Reconstrói a Order a partir do cart de origem, que continua sendo o registro
-- completo daquelas vendas. Idempotente por NOT EXISTS em cada tabela: rodar de
-- novo (ou rodar depois de a materialização já ter criado a Order) é no-op.
--
-- Escopo: carts pagos ou estornados (estornado foi pago um dia — some da tela
-- do mesmo jeito). Carrinho não pago NÃO vira Order: Order é registro de venda.
--
-- Limites conhecidos, por perda de dados na origem:
--   • tracking_token: a coluna foi dropada de carts na 000100, então pedidos
--     antigos ficam sem link público de rastreio (order_logistics.tracking_token
--     nasce NULL). O pedido aparece na tela; só o link do comprador não volta.
--   • erp_payment_snapshot e erp_last_error/attempts: dropados de carts na
--     000101. erp_finalisation_status é inferido de external_order_id — se o
--     pedido de venda existe no ERP, a finalização aconteceu.

-- 1. A raiz.
INSERT INTO orders (
    cart_id, short_id, store_id, event_id, customer_id, status,
    total_cents, discount_cents, shipping_cents, paid_total_cents,
    paid_at, customer_snapshot, created_at, updated_at
)
SELECT
    c.id,
    c.short_id,
    e.store_id,
    c.event_id,
    c.customer_id,
    CASE WHEN c.payment_status = 'refunded' THEN 'refunded' ELSE 'paid' END,
    cart_product_total_cents(c.id),
    COALESCE(c.coupon_discount_cents, 0),
    COALESCE(c.shipping_cost_cents, 0),
    cart_product_total_cents(c.id)
        - COALESCE(c.coupon_discount_cents, 0)
        + COALESCE(c.shipping_cost_cents, 0),
    c.paid_at,
    jsonb_build_object(
        'name',     COALESCE(c.customer_name, ''),
        'email',    COALESCE(c.customer_email, ''),
        'document', COALESCE(c.customer_document, ''),
        'phone',    COALESCE(c.customer_phone, '')
    ),
    -- created_at da Order = quando a venda aconteceu, para o pedido antigo cair
    -- no lugar certo da ordenação por data em vez de aparecer como "agora".
    COALESCE(c.paid_at, c.created_at),
    now()
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE c.payment_status IN ('paid', 'refunded')
  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.cart_id = c.id);

-- 2. Itens (snapshot do nome do produto, igual à materialização em produção).
INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
SELECT o.id, ci.product_id, p.name, ci.quantity, COALESCE(ci.unit_price, 0)
FROM orders o
JOIN cart_items ci ON ci.cart_id = o.cart_id
JOIN products p    ON p.id = ci.product_id
WHERE NOT EXISTS (SELECT 1 FROM order_items oi WHERE oi.order_id = o.id);

-- 3. Pagamento. O snapshot do cartão espelha buildCardSnapshot; gateway_snapshot
--    fica NULL (nunca existiu no cart para essas vendas).
INSERT INTO order_payments (
    order_id, payment_status, payment_method, card_snapshot,
    coupon_id, coupon_code, coupon_discount_cents,
    external_order_id, erp_finalisation_status
)
SELECT
    o.id,
    CASE WHEN c.payment_status = 'refunded' THEN 'refunded' ELSE 'paid' END,
    c.payment_method,
    CASE
        WHEN c.card_brand IS NOT NULL OR c.card_last_four IS NOT NULL THEN
            jsonb_build_object(
                'brand',              COALESCE(c.card_brand, ''),
                'last_four',          COALESCE(c.card_last_four, ''),
                'installments',       COALESCE(c.card_installments, 0),
                'authorization_code', COALESCE(c.card_authorization_code, '')
            )
        ELSE NULL
    END,
    c.coupon_id,
    c.coupon_code,
    COALESCE(c.coupon_discount_cents, 0),
    c.external_order_id,
    CASE WHEN c.external_order_id IS NOT NULL AND c.external_order_id <> ''
         THEN 'done' ELSE 'pending' END
FROM orders o
JOIN carts c ON c.id = o.cart_id
WHERE NOT EXISTS (SELECT 1 FROM order_payments op WHERE op.order_id = o.id);

-- 4. Logística (frete escolhido + estado do pedido no ERP).
INSERT INTO order_logistics (
    order_id, shipping_address, shipping_provider, shipping_service_id,
    shipping_service_name, shipping_carrier, shipping_cost_cents,
    shipping_cost_real_cents, shipping_deadline_days,
    erp_order_state, erp_stock_launched, erp_op_started_at
)
SELECT
    o.id,
    c.shipping_address,
    c.shipping_provider,
    c.shipping_service_id,
    c.shipping_service_name,
    c.shipping_carrier,
    c.shipping_cost_cents,
    c.shipping_cost_real_cents,
    c.shipping_deadline_days,
    COALESCE(c.erp_order_state, 'none'),
    COALESCE(c.erp_stock_launched, false),
    c.erp_op_started_at
FROM orders o
JOIN carts c ON c.id = o.cart_id
WHERE NOT EXISTS (SELECT 1 FROM order_logistics ol WHERE ol.order_id = o.id);
