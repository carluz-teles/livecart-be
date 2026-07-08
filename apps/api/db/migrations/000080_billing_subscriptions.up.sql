-- =============================================================================
-- BILLING / ASSINATURAS STRIPE (PRD 007)
-- =============================================================================
-- A tabela subscriptions do init (000001) era vestigial: amarrada a
-- integration_id (ideia antiga de cobrar via gateway do lojista) e nunca foi
-- consultada por nenhuma query. Remodelada aqui para o billing via Stripe.

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_integration_id_fkey;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS integration_id;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS external_subscription_id;

ALTER TABLE subscriptions
  ADD COLUMN IF NOT EXISTS stripe_customer_id     VARCHAR,
  ADD COLUMN IF NOT EXISTS stripe_subscription_id VARCHAR,
  ADD COLUMN IF NOT EXISTS plan                   VARCHAR NOT NULL DEFAULT 'grow',
  ADD COLUMN IF NOT EXISTS trial_ends_at          TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS cancel_at_period_end   BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS grace_until            TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS manual_override        BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS updated_at             TIMESTAMPTZ DEFAULT NOW();

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_check;
ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_plan_check
  CHECK (plan IN ('start', 'grow', 'scale', 'enterprise'));

-- status: trialing | active | past_due | paused | unpaid | canceled
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_status_check;
ALTER TABLE subscriptions
  ADD CONSTRAINT subscriptions_status_check
  CHECK (status IN ('trialing', 'active', 'past_due', 'paused', 'unpaid', 'canceled'));

-- Uma assinatura por loja.
CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_store ON subscriptions (store_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_stripe_customer ON subscriptions (stripe_customer_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_stripe_sub ON subscriptions (stripe_subscription_id);

-- Backfill: lojas existentes (pre-paywall) ganham um trial de 7 dias a partir
-- de agora — produto ainda nao lancou, so ha lojas de dev/teste. Se alguma
-- precisar de acesso permanente, flipar manual_override = TRUE.
INSERT INTO subscriptions (store_id, status, plan, trial_ends_at, current_period_start, current_period_end)
SELECT s.id, 'trialing', 'grow', NOW() + INTERVAL '7 days', NOW(), NOW() + INTERVAL '7 days'
FROM stores s
WHERE NOT EXISTS (SELECT 1 FROM subscriptions sub WHERE sub.store_id = s.id);
