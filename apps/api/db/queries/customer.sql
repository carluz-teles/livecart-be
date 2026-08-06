-- =============================================================================
-- CUSTOMERS
-- =============================================================================

-- name: CreateCustomer :one
INSERT INTO customers (
    store_id,
    platform_user_id,
    platform_handle,
    email,
    phone,
    first_order_at,
    last_order_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpsertCustomer :one
-- Creates a new customer or updates existing one (by store_id + platform_user_id)
INSERT INTO customers (
    store_id,
    platform_user_id,
    platform_handle,
    email,
    phone,
    first_order_at,
    last_order_at
) VALUES ($1, $2, $3, $4, $5, now(), now())
ON CONFLICT (store_id, platform_user_id) DO UPDATE SET
    platform_handle = COALESCE(EXCLUDED.platform_handle, customers.platform_handle),
    email = COALESCE(EXCLUDED.email, customers.email),
    phone = COALESCE(EXCLUDED.phone, customers.phone),
    last_order_at = now(),
    updated_at = now()
RETURNING *;

-- name: GetCustomerByID :one
SELECT * FROM customers WHERE id = $1;

-- name: GetCustomerByPlatformUser :one
SELECT * FROM customers
WHERE store_id = $1 AND platform_user_id = $2;

-- name: GetCustomerByHandle :one
SELECT * FROM customers
WHERE store_id = $1 AND platform_handle = $2
LIMIT 1;

-- name: UpdateCustomer :exec
UPDATE customers SET
    platform_handle = COALESCE($2, platform_handle),
    email = COALESCE($3, email),
    phone = COALESCE($4, phone),
    updated_at = now()
WHERE id = $1;

-- name: UpdateCustomerLastOrder :exec
UPDATE customers SET
    last_order_at = now(),
    updated_at = now()
WHERE id = $1;

-- name: ListCustomers :many
-- List customers with aggregated order stats
-- Grupo B correction: was summing ALL carts (unpaid included); now counts only paid orders.
SELECT
    c.*,
    COALESCE(stats.total_orders, 0)::INT as total_orders,
    COALESCE(stats.total_spent, 0)::BIGINT as total_spent
FROM customers c
LEFT JOIN LATERAL (
    SELECT
        COUNT(o.id)::INT as total_orders,
        COALESCE(SUM(o.total_cents), 0)::BIGINT as total_spent
    FROM orders o
    WHERE o.customer_id = c.id AND o.status = 'paid'
) stats ON true
WHERE c.store_id = $1
ORDER BY c.last_order_at DESC NULLS LAST
LIMIT $2 OFFSET $3;

-- name: CountCustomers :one
SELECT COUNT(*)::int FROM customers WHERE store_id = $1;

-- name: GetCustomerStats :one
-- Grupo B correction: avg_spent_per_customer was summing ALL carts; now counts only paid orders.
SELECT
    COUNT(*)::INT as total_customers,
    COUNT(CASE WHEN last_order_at > now() - interval '30 days' THEN 1 END)::INT as active_customers,
    COALESCE(
        (
            SELECT SUM(o.total_cents) / NULLIF(COUNT(DISTINCT o.customer_id), 0)
            FROM orders o
            WHERE o.store_id = $1 AND o.status = 'paid'
        ),
        0
    )::BIGINT as avg_spent_per_customer
FROM customers
WHERE store_id = $1;

-- name: SearchCustomers :many
-- Grupo B correction: was summing ALL carts (unpaid included); now counts only paid orders.
SELECT
    c.*,
    COALESCE(stats.total_orders, 0)::INT as total_orders,
    COALESCE(stats.total_spent, 0)::BIGINT as total_spent
FROM customers c
LEFT JOIN LATERAL (
    SELECT
        COUNT(o.id)::INT as total_orders,
        COALESCE(SUM(o.total_cents), 0)::BIGINT as total_spent
    FROM orders o
    WHERE o.customer_id = c.id AND o.status = 'paid'
) stats ON true
WHERE c.store_id = $1
  AND (c.platform_handle ILIKE $2 OR c.email ILIKE $2)
ORDER BY c.last_order_at DESC NULLS LAST
LIMIT $3 OFFSET $4;

-- name: DeleteCustomer :exec
DELETE FROM customers WHERE id = $1;

-- name: GetCustomerCheckoutSnapshot :one
-- Pulls the most-recent non-empty customer/shipping fields from a cart so the
-- detail drawer can show what the buyer filled at checkout (name, document,
-- phone, address) even when the customers table itself is sparse. Prefers
-- paid carts; falls back to any cart with the fields filled.
SELECT
    c.customer_name,
    c.customer_document,
    c.customer_phone,
    c.customer_email,
    c.shipping_address,
    c.created_at
FROM carts c
WHERE c.customer_id = $1
  AND (
    NULLIF(c.customer_name, '') IS NOT NULL
    OR NULLIF(c.customer_document, '') IS NOT NULL
    OR NULLIF(c.customer_phone, '') IS NOT NULL
    OR c.shipping_address IS NOT NULL
  )
ORDER BY
  CASE WHEN c.payment_status = 'paid' THEN 0 ELSE 1 END,
  c.created_at DESC
LIMIT 1;

-- name: SetCustomerWhatsAppOptOutByPhone :execrows
-- Inbound SAIR/PARAR reply on WhatsApp — opt the customer out of future
-- business-initiated messages. Matched by store + phone (E.164).
UPDATE customers
SET whatsapp_opted_out = $3, updated_at = NOW()
WHERE store_id = $1 AND phone = $2;

-- name: IsCustomerWhatsAppOptedOut :one
-- LGPD gate for the reminder fallback: any customer of this store with this
-- phone that replied SAIR/PARAR blocks further business-initiated messages.
SELECT EXISTS(
  SELECT 1 FROM customers
  WHERE store_id = $1 AND phone = $2 AND whatsapp_opted_out = TRUE
) AS opted_out;

-- name: FindDMCapableUserIDByHandle :one
-- O id deste @ na loja que NÃO é o id passado em exclude_user_id, preferindo o
-- que comprovadamente já recebeu DM.
--
-- Existe por causa da loja comentando na PRÓPRIA transmissão. Aí o Instagram
-- não devolve um id de comprador: devolve o id da CONTA. São três identidades
-- para o mesmo @ e só uma serve de destinatário:
--
--   28139217855675836  app-scoped id   (metadata da integração)
--   17841439350112281  conta profissional (entry.id do webhook)
--   1498886768484002   IGSID           — o único que a API de mensagens aceita
--
-- O webhook resolve isso sozinho: traz `from.self_ig_scoped_id` e o handler o
-- prefere. A aresta `/{media}/comments` do polling não tem esse campo, e a doc
-- da Meta é explícita em dizer que o IGSID de auto-conversa vem "from the
-- webhook" — não há endpoint que o recupere. Então recuperamos do que já
-- gravamos: se o webhook já viu essa pessoa uma vez, o IGSID está em customers.
--
-- Medido em 05/08, mesma loja, mesmo evento: a identidade do IGSID entregou 1
-- DM em 1 comentário; a da conta profissional, 0 em 4.
--
-- O desempate por private_reply_used é empírico de propósito — DM entregue é a
-- única prova de que um id é aceito como destinatário.
SELECT cu.platform_user_id
FROM customers cu
WHERE cu.store_id = @store_id
  AND cu.platform_handle = @handle
  AND cu.platform_user_id <> @exclude_user_id::text
ORDER BY (
    SELECT count(*)
    FROM live_comments lc
    JOIN live_events e ON e.id = lc.event_id
    WHERE e.store_id = cu.store_id
      AND lc.platform_user_id = cu.platform_user_id
      AND lc.private_reply_used
  ) DESC,
  cu.last_order_at DESC NULLS LAST
LIMIT 1;
