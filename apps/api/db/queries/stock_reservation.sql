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

-- name: HasActiveEventForProduct :one
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN live_events le ON le.id = sr.event_id
    WHERE sr.external_product_id = $1 AND sr.status = 'active' AND le.status = 'active'
) AS has_active;
