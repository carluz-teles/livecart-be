CREATE TABLE api_rate_budgets (
 account_key text PRIMARY KEY,
 next_at timestamptz NOT NULL DEFAULT now(),
 interval_ms bigint NOT NULL CHECK(interval_ms>0),
 blocked_until timestamptz NOT NULL DEFAULT now()
);
