-- =============================================================================
-- PAYMENTS
-- =============================================================================

-- name: CreatePayment :one
INSERT INTO payments (
    cart_id,
    integration_id,
    external_payment_id,
    provider,
    amount_cents,
    currency,
    method,
    status,
    status_detail,
    provider_response,
    idempotency_key
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING *;

-- name: GetPaymentByID :one
SELECT * FROM payments WHERE id = $1;

-- name: GetPaymentByExternalID :one
SELECT * FROM payments
WHERE external_payment_id = $1
LIMIT 1;

-- name: GetPaymentByIdempotencyKey :one
SELECT * FROM payments
WHERE idempotency_key = $1
LIMIT 1;

-- name: ListPaymentsByCart :many
SELECT * FROM payments
WHERE cart_id = $1
ORDER BY created_at DESC;

-- name: GetLatestPaymentByCart :one
SELECT * FROM payments
WHERE cart_id = $1
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdatePaymentStatus :exec
UPDATE payments
SET status = $2,
    status_detail = $3,
    paid_at = $4,
    updated_at = now()
WHERE id = $1;

-- name: UpdatePaymentByExternalID :exec
UPDATE payments
SET status = $2,
    status_detail = $3,
    paid_at = $4,
    method = COALESCE($5, method),
    provider_response = COALESCE($6, provider_response),
    updated_at = now()
WHERE external_payment_id = $1;

-- name: CountPaymentsByStatus :many
SELECT p.status, COUNT(*)::int as count
FROM payments p
JOIN carts c ON c.id = p.cart_id
JOIN live_events le ON le.id = c.event_id
WHERE le.store_id = $1
GROUP BY p.status;

-- name: ListPaymentsByStore :many
SELECT p.* FROM payments p
JOIN carts c ON c.id = p.cart_id
JOIN live_events le ON le.id = c.event_id
WHERE le.store_id = $1
ORDER BY p.created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetPaymentStats :one
SELECT
    COUNT(*)::int as total_payments,
    COUNT(CASE WHEN p.status = 'approved' THEN 1 END)::int as approved_payments,
    COALESCE(SUM(CASE WHEN p.status = 'approved' THEN p.amount_cents END), 0)::bigint as total_approved_amount
FROM payments p
JOIN carts c ON c.id = p.cart_id
JOIN live_events le ON le.id = c.event_id
WHERE le.store_id = $1;
