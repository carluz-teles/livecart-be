-- =============================================================================
-- ERP CONTACTS (cache of Tiny contact IDs per platform user)
-- =============================================================================

-- name: GetERPContact :one
SELECT * FROM erp_contacts
WHERE store_id = $1 AND integration_id = $2 AND platform_user_id = $3;

-- name: UpsertERPContact :one
INSERT INTO erp_contacts (store_id, integration_id, platform_user_id, platform_handle, external_contact_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (store_id, integration_id, platform_user_id)
DO UPDATE SET
    platform_handle = EXCLUDED.platform_handle,
    external_contact_id = EXCLUDED.external_contact_id,
    updated_at = now()
RETURNING *;

-- name: ListERPContactsByStore :many
SELECT * FROM erp_contacts
WHERE store_id = $1
ORDER BY created_at DESC;
