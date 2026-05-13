-- =============================================================================
-- BLOCKED HANDLES
-- =============================================================================

-- name: BlockHandle :one
-- Idempotent: inserts a new block or reactivates a previously-unblocked row.
INSERT INTO blocked_handles (store_id, platform_handle, reason, blocked_by_user_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (store_id, platform_handle) DO UPDATE
SET reason               = EXCLUDED.reason,
    blocked_by_user_id   = EXCLUDED.blocked_by_user_id,
    blocked_at           = now(),
    unblocked_at         = NULL,
    unblocked_by_user_id = NULL,
    updated_at           = now()
RETURNING *;

-- name: UnblockHandle :one
-- Marks an active block as inactive. Returns the row even if it was already
-- inactive so callers can distinguish "not found" (no row) from "already
-- unblocked" (row returned with unblocked_at not null in this call).
UPDATE blocked_handles
SET unblocked_at         = now(),
    unblocked_by_user_id = $3,
    updated_at           = now()
WHERE store_id = $1 AND platform_handle = $2
RETURNING *;

-- name: IsHandleBlocked :one
SELECT EXISTS(
    SELECT 1 FROM blocked_handles
    WHERE store_id = $1
      AND platform_handle = $2
      AND unblocked_at IS NULL
) AS blocked;

-- name: GetBlockedHandle :one
SELECT * FROM blocked_handles
WHERE store_id = $1 AND platform_handle = $2;

-- name: ListBlockedHandles :many
SELECT * FROM blocked_handles
WHERE store_id = $1
  AND ($2::boolean OR unblocked_at IS NULL)
ORDER BY blocked_at DESC
LIMIT $3 OFFSET $4;

-- name: CountBlockedHandles :one
SELECT COUNT(*)::int AS total
FROM blocked_handles
WHERE store_id = $1
  AND ($2::boolean OR unblocked_at IS NULL);

-- name: ListOpenCartsByHandle :many
-- Returns the carts that still need to be cancelled when the customer is
-- blocked: anything that isn't paid yet. Already-expired/cancelled carts are
-- skipped (the block flow is idempotent).
SELECT c.*
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE e.store_id = $1
  AND c.platform_handle = $2
  AND c.status NOT IN ('expired', 'cancelled')
  AND (c.payment_status IS NULL OR c.payment_status NOT IN ('paid', 'refunded'))
ORDER BY c.created_at DESC;

-- name: CancelCartAsBlocked :one
-- Marks a cart as cancelled because the customer was blocked. Idempotent —
-- only flips carts that haven't been paid.
UPDATE carts
SET status           = 'cancelled',
    cancelled_reason = 'customer_blocked'
WHERE id = $1
  AND status NOT IN ('expired', 'cancelled')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;

-- name: ListBlockedHandlesForStore :many
-- Lookup variant used by the order/customer detail endpoints to surface a
-- "cliente bloqueado" badge for a batch of handles in a single query.
SELECT platform_handle
FROM blocked_handles
WHERE store_id = $1
  AND platform_handle = ANY($2::text[])
  AND unblocked_at IS NULL;
