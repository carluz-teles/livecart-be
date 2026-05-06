-- =============================================================================
-- ORDER EVENTS
-- =============================================================================
-- Customer-facing timeline. Distinct from shipment_tracking_events (carrier
-- raw codes) — these events are the lifeline the public page reads from.

-- name: InsertOrderEvent :one
-- Idempotent insert: ON CONFLICT (cart_id, event_type) DO NOTHING relies on
-- the unique index from migration 000067. Returning xmax = 0 lets the caller
-- distinguish "first emit" from "retry" so duplicate emails are skipped.
INSERT INTO order_events (cart_id, event_type, occurred_at, source, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (cart_id, event_type) DO NOTHING
RETURNING *;

-- name: ListOrderEventsByCart :many
SELECT * FROM order_events
WHERE cart_id = $1
ORDER BY occurred_at ASC;

-- name: HasOrderEvent :one
SELECT EXISTS (
    SELECT 1 FROM order_events
    WHERE cart_id = $1 AND event_type = $2
)::bool AS exists;
