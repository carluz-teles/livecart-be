-- A short renewable lease lets interactive Tiny reads precede catalogue scans.
-- One field per existing account/category; no per-request history or rows.
ALTER TABLE api_rate_budgets
    ADD COLUMN interactive_until timestamptz NOT NULL DEFAULT '-infinity';
