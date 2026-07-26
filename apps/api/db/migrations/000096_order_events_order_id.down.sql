-- Reverte a Fatia C1: volta a idempotência/keying para o cart e remove order_id.
DROP INDEX IF EXISTS idx_order_events_order_type_unique;

CREATE UNIQUE INDEX IF NOT EXISTS idx_order_events_cart_type_unique
    ON order_events (cart_id, event_type);

DROP INDEX IF EXISTS idx_order_events_order_time;

ALTER TABLE order_events DROP COLUMN IF EXISTS order_id;
