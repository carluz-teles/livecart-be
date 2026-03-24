-- Revert FK constraints (back to NO ACTION)

ALTER TABLE integration_logs
DROP CONSTRAINT IF EXISTS integration_logs_integration_id_fkey;

ALTER TABLE integration_logs
ADD CONSTRAINT integration_logs_integration_id_fkey
FOREIGN KEY (integration_id) REFERENCES integrations(id);

ALTER TABLE subscriptions
DROP CONSTRAINT IF EXISTS subscriptions_integration_id_fkey;

ALTER TABLE subscriptions
ADD CONSTRAINT subscriptions_integration_id_fkey
FOREIGN KEY (integration_id) REFERENCES integrations(id);

ALTER TABLE carts
DROP CONSTRAINT IF EXISTS carts_payment_integration_id_fkey;

ALTER TABLE carts
ADD CONSTRAINT carts_payment_integration_id_fkey
FOREIGN KEY (payment_integration_id) REFERENCES integrations(id);
