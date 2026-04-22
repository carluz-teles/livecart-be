-- =============================================================================
-- LIVE SESSIONS (belong to events, platform-agnostic)
-- =============================================================================

-- name: CreateLiveSession :one
INSERT INTO live_sessions (event_id, status)
VALUES ($1, $2)
RETURNING *;

-- name: GetLiveSessionByID :one
SELECT * FROM live_sessions WHERE id = $1;

-- name: GetLiveSessionByIDAndEvent :one
SELECT * FROM live_sessions WHERE id = $1 AND event_id = $2;

-- name: GetActiveSessionByEvent :one
SELECT * FROM live_sessions
WHERE event_id = $1 AND status IN ('active', 'live')
ORDER BY created_at DESC
LIMIT 1;

-- name: StartLiveSession :one
UPDATE live_sessions
SET status = 'live', started_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: EndLiveSession :one
UPDATE live_sessions
SET status = 'ended', ended_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListSessionsByEvent :many
SELECT * FROM live_sessions
WHERE event_id = $1
ORDER BY created_at DESC;

-- name: IncrementLiveSessionComments :exec
UPDATE live_sessions
SET total_comments = total_comments + 1, updated_at = now()
WHERE id = $1;

-- =============================================================================
-- LIVE SESSION PLATFORMS (multiple platform IDs per session)
-- =============================================================================

-- name: AddPlatformToSession :one
INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByPlatformLiveID :one
-- Find active live session by any associated platform_live_id
SELECT ls.*
FROM live_sessions ls
JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE lsp.platform_live_id = $1 AND ls.status IN ('active', 'live')
ORDER BY ls.created_at DESC
LIMIT 1;

-- name: ListPlatformsBySession :many
SELECT * FROM live_session_platforms
WHERE session_id = $1
ORDER BY added_at;

-- name: RemovePlatformFromSession :exec
DELETE FROM live_session_platforms
WHERE session_id = $1 AND platform_live_id = $2;

-- name: CountPlatformsBySession :one
SELECT COUNT(*)::int FROM live_session_platforms WHERE session_id = $1;

-- name: GetPlatformByLiveID :one
SELECT * FROM live_session_platforms WHERE platform_live_id = $1;

