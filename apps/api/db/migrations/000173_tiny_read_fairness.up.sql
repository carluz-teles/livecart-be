-- Keep background recovery moving during continuous interactive lookups.
-- Pending readers renew a short lease; abandoned work cannot block an account.
ALTER TABLE api_rate_budgets
    ADD COLUMN catalog_wait_since timestamptz NOT NULL DEFAULT '-infinity',
    ADD COLUMN catalog_until timestamptz NOT NULL DEFAULT '-infinity';
