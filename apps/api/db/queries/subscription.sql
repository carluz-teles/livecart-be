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
VALUES ($1, 'trialing', 'pro', $2, NOW(), $2)
ON CONFLICT (store_id) DO UPDATE SET updated_at = NOW()
RETURNING *;

-- name: DeleteSubscriptionsByStore :exec
-- subscriptions.store_id NÃO tem ON DELETE CASCADE, então o trial criado no
-- onboarding bloqueia o DELETE da loja. Usado só ao descartar loja vazia no
-- aceite de convite — nunca há histórico de cobrança a preservar ali.
DELETE FROM subscriptions WHERE store_id = $1;

-- name: SetSubscriptionStripeRefs :one
-- Grava os IDs Stripe apos criar customer/subscription remotos.
UPDATE subscriptions
SET stripe_customer_id     = COALESCE($2, stripe_customer_id),
    stripe_subscription_id = COALESCE($3, stripe_subscription_id),
    updated_at             = NOW()
WHERE store_id = $1
RETURNING *;

-- name: SetSubscriptionInterval :exec
-- Grava o intervalo de cobrança escolhido (mensal/semestral/anual) na
-- conversão. A troca posterior entre intervalos é feita pelo Customer Portal
-- (os 3 preços do produto Pro agrupados lá) — não há endpoint próprio.
UPDATE subscriptions
SET billing_interval = $2,
    updated_at        = NOW()
WHERE store_id = $1;

-- name: ListLegacyPlanSubscriptions :many
-- Assinaturas ainda nos planos antigos (start/grow/scale), candidatas à
-- migração para o plano único "pro". Usado pelo job one-off de migração.
SELECT * FROM subscriptions
WHERE plan IN ('start', 'grow', 'scale')
  AND status IN ('trialing', 'active', 'past_due')
ORDER BY store_id;

-- name: MigrateSubscriptionToPro :exec
-- Aplica a migração local (plano + intervalo) depois da troca de price ter
-- sido confirmada no Stripe pelo job de migração.
UPDATE subscriptions
SET plan             = 'pro',
    billing_interval = $2,
    updated_at       = NOW()
WHERE store_id = $1;

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

-- name: SumUnbilledBillableFees :one
-- Soma a taxa (líquida de refunds via refund_credit) ainda NÃO faturada de uma
-- loja, até o corte do ciclo. stripe_ref IS NULL = não faturada. Usado pelo
-- reactor de ciclo pra montar o InvoiceItem da comissão.
SELECT COALESCE(SUM(fee_cents), 0)::bigint AS fee_cents
FROM billing_ledger_entries
WHERE store_id = $1 AND billable = true AND stripe_ref IS NULL AND created_at < $2;

-- name: MarkStoreFeesInvoiced :exec
-- Marca como faturadas as taxas billable não faturadas até o corte (idempotência
-- do ciclo: um redelivery não refatura). Mesmo predicado do SUM.
UPDATE billing_ledger_entries
SET stripe_ref = $3
WHERE store_id = $1 AND billable = true AND stripe_ref IS NULL AND created_at < $2;

-- name: InsertTaxaPromo :one
-- Cria uma promo de desconto sobre a comissão (taxa) para uma loja.
INSERT INTO billing_taxa_promos (store_id, discount_bps, cycles_remaining, code, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetActiveTaxaPromo :one
-- Promo de taxa ativa da loja (mais recente com ciclos restantes). Usada pelo
-- reactor de ciclo pra descontar a comissão antes de faturar.
SELECT * FROM billing_taxa_promos
WHERE store_id = $1 AND cycles_remaining > 0
ORDER BY created_at DESC
LIMIT 1;

-- name: ConsumeTaxaPromoCycle :exec
-- Consome um ciclo da promo (decrementa) após aplicá-la numa fatura.
UPDATE billing_taxa_promos
SET cycles_remaining = cycles_remaining - 1
WHERE id = $1 AND cycles_remaining > 0;

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
