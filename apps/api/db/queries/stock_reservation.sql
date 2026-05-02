-- name: CreateStockReservation :one
INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListActiveReservationsByEvent :many
SELECT * FROM stock_reservations WHERE event_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCart :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCartAndProduct :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND product_id = $2 AND status = 'active' ORDER BY created_at ASC;

-- name: ReverseReservationsByCart :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND status = 'active';

-- name: ReverseReservationsByCartAndProduct :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND product_id = $2 AND status = 'active';

-- name: ConvertReservationsByEvent :exec
UPDATE stock_reservations SET status = 'converted', reversed_at = now()
WHERE event_id = $1 AND status = 'active';

-- name: AdjustActiveReservationQuantity :one
-- Atomically increase or decrease the active reservation quantity for a
-- (cart, product) pair. delta_qty can be negative. Returns the row whose
-- quantity was adjusted (callers can flip status when it hits zero).
UPDATE stock_reservations
SET quantity = quantity + sqlc.arg(delta_qty)::int,
    erp_movement_id = COALESCE(NULLIF(sqlc.arg(erp_movement_id)::text, ''), erp_movement_id)
WHERE cart_id = sqlc.arg(cart_id) AND product_id = sqlc.arg(product_id) AND status = 'active'
RETURNING *;

-- name: ReverseExhaustedReservation :exec
-- Marks an active reservation as reversed when its quantity has been
-- adjusted down to zero (cart item fully removed).
UPDATE stock_reservations
SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND product_id = $2 AND status = 'active' AND quantity <= 0;

-- name: HasActiveEventForProduct :one
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN live_events le ON le.id = sr.event_id
    WHERE sr.external_product_id = $1 AND sr.status = 'active' AND le.status = 'active'
) AS has_active;
