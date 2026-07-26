-- =============================================================================
-- ORDER EVENTS
-- =============================================================================
-- Customer-facing timeline. Distinct from shipment_tracking_events (carrier
-- raw codes) — these events are the lifeline the public page reads from.

-- name: InsertOrderEvent :one
-- Idempotent insert: ON CONFLICT (order_id, event_type) DO NOTHING relies on
-- the unique index from migration 000096 (a timeline pertence à Order). cart_id
-- ainda é gravado (dual-key) até a Fase F. A row retornada (ou a ausência dela,
-- ErrNoRows) deixa o caller distinguir "first emit" de "retry" e pular e-mails
-- duplicados.
INSERT INTO order_events (order_id, cart_id, event_type, occurred_at, source, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (order_id, event_type) DO NOTHING
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
