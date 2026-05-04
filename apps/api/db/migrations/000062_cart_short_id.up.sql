-- Human-readable, store-scoped order numbers. Backed by a per-store counter
-- table so concurrent cart creations can bump the next value atomically with a
-- single UPSERT. The first short_id in each store is 1000 — chosen so a
-- brand-new merchant doesn't have to read out "Pedido 1" to a customer on the
-- phone, and so volume cannot be inferred from the absolute number.

CREATE TABLE IF NOT EXISTS store_order_counters (
    store_id   UUID PRIMARY KEY REFERENCES stores(id) ON DELETE CASCADE,
    last_value INTEGER NOT NULL DEFAULT 999
);

ALTER TABLE carts ADD COLUMN IF NOT EXISTS short_id INTEGER;

-- Backfill existing carts: 1000, 1001, ... per store, ordered by created_at
-- ascending so the oldest order in each store gets #1000.
WITH numbered AS (
    SELECT
        c.id AS cart_id,
        e.store_id,
        999 + ROW_NUMBER() OVER (PARTITION BY e.store_id ORDER BY c.created_at, c.id) AS new_short_id
    FROM carts c
    JOIN live_events e ON e.id = c.event_id
)
UPDATE carts c
SET short_id = numbered.new_short_id
FROM numbered
WHERE numbered.cart_id = c.id;

-- Seed the counter to the highest short_id we just assigned per store. The
-- next cart created will UPSERT this row with last_value + 1 and use that
-- value. Stores with no carts get no row here — the bump query inserts with
-- last_value = 1000 on first call (see cart.sql BumpStoreOrderCounter).
INSERT INTO store_order_counters (store_id, last_value)
SELECT e.store_id, MAX(c.short_id)
FROM carts c
JOIN live_events e ON e.id = c.event_id
GROUP BY e.store_id;

ALTER TABLE carts ALTER COLUMN short_id SET NOT NULL;
CREATE INDEX IF NOT EXISTS carts_short_id_idx ON carts (short_id);
