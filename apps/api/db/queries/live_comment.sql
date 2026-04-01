-- =============================================================================
-- LIVE COMMENTS
-- =============================================================================

-- name: CreateLiveComment :one
INSERT INTO live_comments (
    session_id, event_id, platform, platform_comment_id,
    platform_user_id, platform_handle, text,
    has_purchase_intent, matched_product_id, matched_quantity, result
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: UpdateLiveCommentResult :exec
UPDATE live_comments
SET has_purchase_intent = $2,
    matched_product_id = $3,
    matched_quantity = $4,
    result = $5
WHERE id = $1;

-- name: ListCommentsBySession :many
SELECT * FROM live_comments
WHERE session_id = $1
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: ListCommentsByEvent :many
SELECT * FROM live_comments
WHERE event_id = $1
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: ListCommentsByUser :many
SELECT * FROM live_comments
WHERE event_id = $1 AND platform_user_id = $2
ORDER BY created_at;

-- name: CountCommentsBySession :one
SELECT COUNT(*)::int FROM live_comments WHERE session_id = $1;

-- name: CountCommentsByEvent :one
SELECT COUNT(*)::int FROM live_comments WHERE event_id = $1;

-- name: ListPurchaseCommentsByEvent :many
SELECT lc.*, p.name AS product_name, p.keyword AS product_keyword
FROM live_comments lc
LEFT JOIN products p ON p.id = lc.matched_product_id
WHERE lc.event_id = $1 AND lc.has_purchase_intent = true
ORDER BY lc.created_at;
