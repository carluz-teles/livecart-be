-- name: CreateStore :one
INSERT INTO stores (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetStoreByID :one
SELECT * FROM stores WHERE id = $1;

-- name: GetStoreBySlug :one
SELECT * FROM stores WHERE slug = $1;

-- name: UpdateStore :one
UPDATE stores
SET
  name = $2,
  whatsapp_number = $3,
  email_address = $4,
  sms_number = $5,
  description = $6,
  website = $7,
  logo_url = $8,
  address_street = $9,
  address_city = $10,
  address_state = $11,
  address_zip = $12,
  address_country = $13,
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

-- name: ListStoreMembers :many
SELECT * FROM memberships WHERE store_id = $1 ORDER BY created_at;

-- name: GetStoreByUserID :one
-- Get first store for a user (for backwards compatibility)
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.user_id = $1 AND m.status = 'active'
ORDER BY m.last_accessed_at DESC NULLS LAST, m.created_at ASC
LIMIT 1;

-- name: GetStoreByOwnerUserID :one
-- Get store where user is owner
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.user_id = $1 AND m.role = 'owner'
LIMIT 1;

-- name: DeleteStore :exec
DELETE FROM stores WHERE id = $1;
