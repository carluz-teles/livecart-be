-- =============================================================================
-- SUBSCRIPTIONS (Stripe billing — PRD 007)
-- =============================================================================

-- name: GetSubscriptionByStoreID :one
SELECT * FROM subscriptions WHERE store_id = $1;

-- name: GetSubscriptionByStripeSubID :one
SELECT * FROM subscriptions WHERE stripe_subscription_id = $1;

-- name: GetSubscriptionByStripeCustomerID :one
SELECT * FROM subscriptions WHERE stripe_customer_id = $1;

-- name: EnsureTrialSubscription :one
-- Cria o trial local na criacao da loja (ou lazy no /users/sync). Idempotente:
-- se ja existe assinatura para a loja, devolve a existente sem tocar em nada.
INSERT INTO subscriptions (store_id, status, plan, trial_ends_at, current_period_start, current_period_end)
VALUES ($1, 'trialing', 'grow', $2, NOW(), $2)
ON CONFLICT (store_id) DO UPDATE SET updated_at = NOW()
RETURNING *;

-- name: SetSubscriptionStripeRefs :one
-- Grava os IDs Stripe apos criar customer/subscription remotos.
UPDATE subscriptions
SET stripe_customer_id     = COALESCE($2, stripe_customer_id),
    stripe_subscription_id = COALESCE($3, stripe_subscription_id),
    updated_at             = NOW()
WHERE store_id = $1
RETURNING *;

-- name: UpdateSubscriptionFromStripe :one
-- Aplica o estado vindo dos webhooks (fonte da verdade).
UPDATE subscriptions
SET status               = $2,
    plan                 = $3,
    trial_ends_at        = COALESCE($4, trial_ends_at),
    current_period_start = COALESCE($5, current_period_start),
    current_period_end   = COALESCE($6, current_period_end),
    cancel_at_period_end = $7,
    grace_until          = $8,
    updated_at           = NOW()
WHERE stripe_subscription_id = $1
RETURNING *;

-- =============================================================================
-- GMV LEDGER (append-only — PRD 007)
-- =============================================================================

-- name: InsertLedgerEntry :one
-- Idempotente por (cart_id, entry_type): retry de webhook nao duplica.
INSERT INTO billing_ledger_entries (store_id, cart_id, entry_type, amount_cents, plan, fee_bps, fee_cents, billable, stripe_ref)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (cart_id, entry_type) DO NOTHING
RETURNING *;

-- name: GetLedgerSaleEntry :one
SELECT * FROM billing_ledger_entries WHERE cart_id = $1 AND entry_type = 'sale';

-- name: SetLedgerEntryStripeRef :exec
UPDATE billing_ledger_entries SET stripe_ref = $2 WHERE id = $1;

-- name: GetLedgerUsageSince :one
-- Resumo do ciclo (somas assinadas): GMV liquido, taxa liquida, contagens.
SELECT
  COALESCE(SUM(amount_cents), 0)::bigint                                          AS gmv_cents,
  COALESCE(SUM(CASE WHEN billable THEN fee_cents ELSE 0 END), 0)::bigint          AS fee_cents,
  COUNT(*) FILTER (WHERE entry_type = 'sale')::int                                AS sales,
  COUNT(*) FILTER (WHERE entry_type = 'refund_credit')::int                       AS refunds,
  COALESCE(SUM(CASE WHEN entry_type = 'refund_credit' AND billable THEN -fee_cents ELSE 0 END), 0)::bigint AS refund_credits_cents
FROM billing_ledger_entries
WHERE store_id = $1 AND created_at >= $2;

-- name: ListLedgerEntries :many
-- Extrato do lojista, mais recente primeiro, com contexto do pedido.
SELECT
  le.id, le.entry_type, le.amount_cents, le.fee_cents, le.fee_bps, le.billable, le.created_at,
  le.cart_id,
  c.platform_handle,
  c.customer_name
FROM billing_ledger_entries le
JOIN carts c ON c.id = le.cart_id
WHERE le.store_id = $1
ORDER BY le.created_at DESC
LIMIT $2 OFFSET $3;
