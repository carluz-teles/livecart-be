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
SET name = $2, whatsapp_number = $3, email_address = $4, sms_number = $5
WHERE id = $1
RETURNING *;

-- name: ListStoreUsers :many
SELECT * FROM store_users WHERE store_id = $1 ORDER BY created_at;

-- name: CreateStoreUser :one
INSERT INTO store_users (store_id, email, role, password_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;
