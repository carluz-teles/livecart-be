-- Revert billing subscriptions (PRD 007)
DROP INDEX IF EXISTS idx_subscriptions_stripe_sub;
DROP INDEX IF EXISTS idx_subscriptions_stripe_customer;
DROP INDEX IF EXISTS idx_subscriptions_store;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_status_check;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_check;
ALTER TABLE subscriptions
  DROP COLUMN IF EXISTS updated_at,
  DROP COLUMN IF EXISTS manual_override,
  DROP COLUMN IF EXISTS grace_until,
  DROP COLUMN IF EXISTS cancel_at_period_end,
  DROP COLUMN IF EXISTS trial_ends_at,
  DROP COLUMN IF EXISTS plan,
  DROP COLUMN IF EXISTS stripe_subscription_id,
  DROP COLUMN IF EXISTS stripe_customer_id;
ALTER TABLE subscriptions
  ADD COLUMN IF NOT EXISTS integration_id UUID,
  ADD COLUMN IF NOT EXISTS external_subscription_id VARCHAR;
