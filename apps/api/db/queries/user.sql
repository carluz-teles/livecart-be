-- name: GetMembershipsByClerkID :many
-- List all memberships (stores) for a clerk user
SELECT
  m.id,
  m.store_id,
  m.clerk_user_id,
  m.email,
  m.name,
  m.avatar_url,
  m.role,
  m.status,
  m.last_accessed_at,
  m.created_at,
  m.updated_at,
  s.name as store_name,
  s.slug as store_slug,
  s.clerk_org_id
FROM memberships m
JOIN stores s ON s.id = m.store_id
WHERE m.clerk_user_id = $1 AND m.status = 'active'
ORDER BY m.last_accessed_at DESC NULLS LAST, m.created_at ASC;

-- name: GetMembershipByClerkIDAndStore :one
-- Get membership for a specific store
SELECT
  m.id,
  m.store_id,
  m.clerk_user_id,
  m.email,
  m.name,
  m.avatar_url,
  m.role,
  m.status,
  m.last_accessed_at,
  m.created_at,
  m.updated_at,
  s.name as store_name,
  s.slug as store_slug,
  s.clerk_org_id
FROM memberships m
JOIN stores s ON s.id = m.store_id
WHERE m.clerk_user_id = $1 AND m.store_id = $2;

-- name: CreateMembership :one
-- Create a membership (user joins a store)
INSERT INTO memberships (store_id, clerk_user_id, email, name, avatar_url, role, status, invited_by, invited_at)
VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8)
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, last_accessed_at, created_at, updated_at;

-- name: CreateOwnerMembership :one
-- Create owner membership when creating a new store
INSERT INTO memberships (store_id, clerk_user_id, email, name, avatar_url, role, status)
VALUES ($1, $2, $3, $4, $5, 'owner', 'active')
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, last_accessed_at, created_at, updated_at;

-- name: UpdateMembershipLastAccessed :exec
-- Update last accessed timestamp for a membership
UPDATE memberships
SET last_accessed_at = now()
WHERE clerk_user_id = $1 AND store_id = $2;

-- name: UpdateMembership :one
UPDATE memberships
SET
  email = $3,
  name = $4,
  avatar_url = $5,
  updated_at = now()
WHERE clerk_user_id = $1 AND store_id = $2
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, last_accessed_at, created_at, updated_at;

-- name: UpdateMembershipRole :one
UPDATE memberships
SET role = $3, updated_at = now()
WHERE store_id = $1 AND id = $2
RETURNING id, store_id, clerk_user_id, email, name, avatar_url, role, status, last_accessed_at, created_at, updated_at;

-- name: DeleteMembership :exec
DELETE FROM memberships WHERE store_id = $1 AND id = $2;

-- name: UpdateMembershipAllStores :exec
-- Update user info across all memberships (for Clerk webhook)
UPDATE memberships
SET
  email = $2,
  name = $3,
  avatar_url = $4,
  updated_at = now()
WHERE clerk_user_id = $1;

-- name: DeleteMembershipsByClerkID :exec
-- Delete all memberships for a clerk user (when Clerk user is deleted)
DELETE FROM memberships WHERE clerk_user_id = $1;

-- name: GetStoreMembers :many
-- List all members of a store
SELECT
  m.id,
  m.store_id,
  m.clerk_user_id,
  m.email,
  m.name,
  m.avatar_url,
  m.role,
  m.status,
  m.invited_by,
  m.invited_at,
  m.created_at,
  m.updated_at
FROM memberships m
WHERE m.store_id = $1
ORDER BY
  CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
  m.created_at ASC;

-- name: CountStoreMembers :one
SELECT COUNT(*) FROM memberships WHERE store_id = $1 AND status = 'active';

-- name: ValidateStoreAccess :one
-- Check if user has access to a store and return their membership info
SELECT
  m.id,
  m.role
FROM memberships m
WHERE m.clerk_user_id = $1 AND m.store_id = $2 AND m.status = 'active';

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
JOIN memberships inviter ON inviter.id = si.invited_by
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
JOIN memberships inviter ON inviter.id = si.invited_by
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
