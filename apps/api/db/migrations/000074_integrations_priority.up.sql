-- Priority ordering for payment integrations (and any future type that allows
-- multiple per store). Lower number = higher priority. The checkout query
-- picks the primary by ORDER BY priority ASC, created_at DESC, so older rows
-- win the tie when priorities match — until the merchant explicitly reorders.
ALTER TABLE integrations
  ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;

-- Backfill: existing payment rows are spread to unique priorities (10, 20, …)
-- by created_at order so the previous "oldest wins" silent behavior is
-- preserved. Non-payment types stay at 100 — they don't need ordering today.
WITH ranked AS (
  SELECT
    id,
    ROW_NUMBER() OVER (PARTITION BY store_id ORDER BY created_at ASC) * 10 AS new_priority
  FROM integrations
  WHERE type = 'payment'
)
UPDATE integrations i
SET priority = ranked.new_priority
FROM ranked
WHERE i.id = ranked.id;

-- Index that the checkout config endpoint hits on every cart load.
CREATE INDEX IF NOT EXISTS idx_integrations_store_payment_priority
  ON integrations (store_id, priority ASC, created_at DESC)
  WHERE type = 'payment' AND status = 'active';
