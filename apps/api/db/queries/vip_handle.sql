-- =============================================================================
-- VIP HANDLES — clientes cujo carrinho nunca expira (oposto de blocked_handles)
-- =============================================================================

-- name: AddVipHandle :one
-- Idempotente: cria o VIP ou reativa uma linha antes removida.
INSERT INTO vip_handles (store_id, platform_handle, added_by_user_id)
VALUES ($1, $2, $3)
ON CONFLICT (store_id, platform_handle) DO UPDATE
SET added_by_user_id  = EXCLUDED.added_by_user_id,
    added_at          = now(),
    removed_at        = NULL,
    removed_by_user_id = NULL,
    updated_at        = now()
RETURNING *;

-- name: RemoveVipHandle :one
UPDATE vip_handles
SET removed_at         = now(),
    removed_by_user_id = $3,
    updated_at         = now()
WHERE store_id = $1 AND platform_handle = $2
RETURNING *;

-- name: IsVipHandle :one
SELECT EXISTS(
    SELECT 1 FROM vip_handles
    WHERE store_id = $1
      AND platform_handle = $2
      AND removed_at IS NULL
) AS is_vip;

-- name: GetVipHandle :one
SELECT * FROM vip_handles
WHERE store_id = $1 AND platform_handle = $2;

-- name: ListVipHandles :many
SELECT * FROM vip_handles
WHERE store_id = $1
  AND ($2::boolean OR removed_at IS NULL)
ORDER BY added_at DESC
LIMIT $3 OFFSET $4;

-- name: CountVipHandles :one
SELECT COUNT(*)::int AS total
FROM vip_handles
WHERE store_id = $1
  AND ($2::boolean OR removed_at IS NULL);

-- name: ListVipHandlesForStore :many
SELECT platform_handle
FROM vip_handles
WHERE store_id = $1
  AND platform_handle = ANY($2::text[])
  AND removed_at IS NULL;
