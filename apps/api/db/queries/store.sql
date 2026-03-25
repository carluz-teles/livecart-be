-- name: CreateStore :one
INSERT INTO stores (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: CreateStoreWithClerkOrg :one
INSERT INTO stores (name, slug, clerk_org_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetStoreByID :one
SELECT * FROM stores WHERE id = $1;

-- name: GetStoreBySlug :one
SELECT * FROM stores WHERE slug = $1;

-- name: GetStoreByClerkOrgID :one
SELECT * FROM stores WHERE clerk_org_id = $1;

-- name: UpdateStore :one
UPDATE stores
SET
  name = $2,
  whatsapp_number = $3,
  email_address = $4,
  sms_number = $5,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateStoreCartSettings :one
UPDATE stores
SET
  cart_enabled = $2,
  cart_expiration_minutes = $3,
  cart_reserve_stock = $4,
  cart_max_items = $5,
  cart_max_quantity_per_item = $6,
  cart_notify_before_expiration = $7,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateStoreClerkOrgID :one
UPDATE stores
SET clerk_org_id = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListStoreMembers :many
SELECT * FROM memberships WHERE store_id = $1 ORDER BY created_at;

-- name: GetStoreByClerkUserID :one
-- Get first store for a clerk user (for backwards compatibility)
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.clerk_user_id = $1 AND m.status = 'active'
ORDER BY m.last_accessed_at DESC NULLS LAST, m.created_at ASC
LIMIT 1;

-- name: GetStoreByOwnerClerkUserID :one
-- Get store where clerk user is owner
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.clerk_user_id = $1 AND m.role = 'owner'
LIMIT 1;

-- name: DeleteStore :exec
DELETE FROM stores WHERE id = $1;
