-- name: GetUserByClerkID :one
-- Get user's default/first store (for backwards compatibility)
SELECT
  su.id,
  su.store_id,
  su.clerk_user_id,
  su.email,
  su.name,
  su.avatar_url,
  su.role,
  su.status,
  su.created_at,
  su.updated_at,
  s.name as store_name,
  s.slug as store_slug
FROM store_users su
JOIN stores s ON s.id = su.store_id
WHERE su.clerk_user_id = $1 AND su.status = 'active'
ORDER BY su.created_at ASC
LIMIT 1;

-- name: GetUserByClerkIDAndStore :one
-- Get user for a specific store
SELECT
  su.id,
  su.store_id,
  su.clerk_user_id,
  su.email,
  su.name,
  su.avatar_url,
  su.role,
  su.status,
  su.created_at,
  su.updated_at,
  s.name as store_name,
  s.slug as store_slug
FROM store_users su
JOIN stores s ON s.id = su.store_id
WHERE su.clerk_user_id = $1 AND su.store_id = $2;

-- name: GetUserStores :many
-- List all stores a user belongs to
SELECT
  su.id,
  su.store_id,
  su.clerk_user_id,
  su.email,
  su.name,
  su.avatar_url,
  su.role,
  su.status,
  su.created_at,
  su.updated_at,
  s.name as store_name,
  s.slug as store_slug
FROM store_users su
JOIN stores s ON s.id = su.store_id
WHERE su.clerk_user_id = $1
ORDER BY su.created_at ASC;

-- name: CreateUserWithStore :one
WITH new_store AS (
  INSERT INTO stores (name, slug)
  VALUES ($1, $2)
  RETURNING id, name, slug
),
new_user AS (
  INSERT INTO store_users (store_id, clerk_user_id, email, name, avatar_url, role, status)
  SELECT id, $3, $4, $5, $6, 'owner', 'active'
  FROM new_store
  RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, created_at, updated_at
)
SELECT
  new_user.id,
  new_user.store_id,
  new_user.clerk_user_id,
  new_user.email,
  new_user.name,
  new_user.avatar_url,
  new_user.role,
  new_user.status,
  new_user.created_at,
  new_user.updated_at,
  new_store.name as store_name,
  new_store.slug as store_slug
FROM new_user, new_store;

-- name: AddUserToStore :one
-- Add an existing Clerk user to a store (for accepting invitations)
INSERT INTO store_users (store_id, clerk_user_id, email, name, avatar_url, role, status, invited_by, invited_at)
VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, now())
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, created_at, updated_at;

-- name: UpdateUser :one
UPDATE store_users
SET
  email = COALESCE($3, email),
  name = COALESCE($4, name),
  avatar_url = COALESCE($5, avatar_url),
  updated_at = now()
WHERE clerk_user_id = $1 AND store_id = $2
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, created_at, updated_at;

-- name: UpdateUserRole :one
UPDATE store_users
SET role = $3, updated_at = now()
WHERE store_id = $1 AND id = $2
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, created_at, updated_at;

-- name: RemoveUserFromStore :exec
DELETE FROM store_users WHERE store_id = $1 AND id = $2;

-- name: UpdateUserAllStores :exec
-- Update user info across all stores (for Clerk webhook)
UPDATE store_users
SET
  email = COALESCE($2, email),
  name = COALESCE($3, name),
  avatar_url = COALESCE($4, avatar_url),
  updated_at = now()
WHERE clerk_user_id = $1;

-- name: DeleteUserByClerkID :exec
-- Delete user from all stores (when Clerk user is deleted)
DELETE FROM store_users WHERE clerk_user_id = $1;

-- name: GetStoreMembers :many
-- List all members of a store
SELECT
  su.id,
  su.store_id,
  su.clerk_user_id,
  su.email,
  su.name,
  su.avatar_url,
  su.role,
  su.status,
  su.invited_by,
  su.invited_at,
  su.created_at,
  su.updated_at
FROM store_users su
WHERE su.store_id = $1
ORDER BY
  CASE su.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
  su.created_at ASC;

-- name: CountStoreMembers :one
SELECT COUNT(*) FROM store_users WHERE store_id = $1 AND status = 'active';

-- name: ValidateStoreAccess :one
-- Check if user has access to a store and return their store_user info
SELECT
  su.id,
  su.role
FROM store_users su
WHERE su.clerk_user_id = $1 AND su.store_id = $2 AND su.status = 'active';

-- ============================================
-- INVITATION QUERIES
-- ============================================

-- name: CreateInvitation :one
INSERT INTO store_invitations (store_id, email, role, token, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, store_id, email, role, token, invited_by, status, expires_at, created_at;

-- name: GetInvitationByToken :one
SELECT
  si.id,
  si.store_id,
  si.email,
  si.role,
  si.token,
  si.invited_by,
  si.status,
  si.expires_at,
  si.accepted_at,
  si.created_at,
  s.name as store_name,
  s.slug as store_slug,
  inviter.name as inviter_name
FROM store_invitations si
JOIN stores s ON s.id = si.store_id
JOIN store_users inviter ON inviter.id = si.invited_by
WHERE si.token = $1;

-- name: GetInvitationByEmail :one
SELECT
  si.id,
  si.store_id,
  si.email,
  si.role,
  si.token,
  si.invited_by,
  si.status,
  si.expires_at,
  si.accepted_at,
  si.created_at
FROM store_invitations si
WHERE si.store_id = $1 AND si.email = $2 AND si.status = 'pending';

-- name: ListStoreInvitations :many
SELECT
  si.id,
  si.store_id,
  si.email,
  si.role,
  si.token,
  si.invited_by,
  si.status,
  si.expires_at,
  si.accepted_at,
  si.created_at,
  inviter.name as inviter_name
FROM store_invitations si
JOIN store_users inviter ON inviter.id = si.invited_by
WHERE si.store_id = $1
ORDER BY si.created_at DESC;

-- name: AcceptInvitation :one
UPDATE store_invitations
SET status = 'accepted', accepted_at = now()
WHERE id = $1 AND status = 'pending'
RETURNING id, store_id, email, role, token, invited_by, status, expires_at, accepted_at, created_at;

-- name: RevokeInvitation :exec
UPDATE store_invitations
SET status = 'revoked'
WHERE store_id = $1 AND id = $2 AND status = 'pending';

-- name: ExpireInvitations :exec
-- Mark expired invitations (run periodically)
UPDATE store_invitations
SET status = 'expired'
WHERE status = 'pending' AND expires_at < now();

-- name: DeleteInvitation :exec
DELETE FROM store_invitations WHERE store_id = $1 AND id = $2;
