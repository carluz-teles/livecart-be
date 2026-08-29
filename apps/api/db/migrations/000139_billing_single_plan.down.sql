ALTER TABLE subscriptions ALTER COLUMN plan SET DEFAULT 'grow';

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_check
  CHECK (plan IN ('start', 'grow', 'scale', 'enterprise'));

ALTER TABLE subscriptions DROP COLUMN IF EXISTS billing_interval;
