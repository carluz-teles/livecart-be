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
