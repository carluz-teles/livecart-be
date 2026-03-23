-- name: GetUserByClerkID :one
SELECT
  su.id,
  su.store_id,
  su.clerk_user_id,
  su.email,
  su.name,
  su.avatar_url,
  su.role,
  su.created_at,
  su.updated_at,
  s.name as store_name,
  s.slug as store_slug
FROM store_users su
JOIN stores s ON s.id = su.store_id
WHERE su.clerk_user_id = $1;

-- name: CreateUserWithStore :one
WITH new_store AS (
  INSERT INTO stores (name, slug)
  VALUES ($1, $2)
  RETURNING id, name, slug
),
new_user AS (
  INSERT INTO store_users (store_id, clerk_user_id, email, name, avatar_url, role)
  SELECT id, $3, $4, $5, $6, 'owner'
  FROM new_store
  RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, created_at, updated_at
)
SELECT
  new_user.id,
  new_user.store_id,
  new_user.clerk_user_id,
  new_user.email,
  new_user.name,
  new_user.avatar_url,
  new_user.role,
  new_user.created_at,
  new_user.updated_at,
  new_store.name as store_name,
  new_store.slug as store_slug
FROM new_user, new_store;

-- name: UpdateUser :one
UPDATE store_users
SET
  email = COALESCE($2, email),
  name = COALESCE($3, name),
  avatar_url = COALESCE($4, avatar_url),
  updated_at = now()
WHERE clerk_user_id = $1
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, created_at, updated_at;

-- name: DeleteUserByClerkID :exec
DELETE FROM store_users WHERE clerk_user_id = $1;
