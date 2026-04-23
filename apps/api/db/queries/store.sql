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
  cnpj = $14,
  address_number = $15,
  address_complement = $16,
  address_district = $17,
  address_state_register = $18,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateStoreCartSettings :one
UPDATE stores
SET
  cart_enabled = $2,
  cart_expiration_minutes = $3,
  cart_reserve_stock = $4,
  cart_max_quantity_per_item = $5,
  cart_allow_edit = $6,
  cart_real_time = $7,
  send_on_live_end = $8,
  checkout_send_methods = $9,
  cart_message_cooldown_seconds = $10,
  cart_send_expiration_reminder = $11,
  cart_expiration_reminder_minutes = $12,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateStoreShippingDefaults :one
UPDATE stores
SET
  default_package_weight_grams = $2,
  default_package_format       = $3,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListStoreMembers :many
SELECT * FROM memberships WHERE store_id = $1 ORDER BY created_at;

-- name: GetStoreByUserID :one
-- Get the single store for a user (1 user = 1 store)
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.user_id = $1 AND m.status = 'active';

-- name: GetStoreByOwnerUserID :one
-- Get store where user is owner
SELECT s.*
FROM stores s
JOIN memberships m ON s.id = m.store_id
WHERE m.user_id = $1 AND m.role = 'owner'
LIMIT 1;

-- name: DeleteStore :exec
DELETE FROM stores WHERE id = $1;

-- name: UpdateStoreCheckoutSettings :one
UPDATE stores
SET
  checkout_send_methods = $2,
  send_on_live_end = $3,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateStoreLogoURL :one
UPDATE stores
SET
  logo_url = $2,
  updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetStoreNameByID :one
SELECT name, cart_expiration_minutes, cart_max_quantity_per_item FROM stores WHERE id = $1;
