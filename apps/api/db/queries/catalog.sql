-- name: CreateCatalog :one
INSERT INTO catalogs (store_id, name)
VALUES ($1, $2)
RETURNING *;

-- name: GetCatalogByID :one
SELECT * FROM catalogs WHERE id = $1 AND store_id = $2;

-- name: ListCatalogsByStore :many
SELECT c.*, COUNT(cp.product_id) AS product_count
FROM catalogs c
LEFT JOIN catalog_products cp ON cp.catalog_id = c.id
WHERE c.store_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC;

-- name: UpdateCatalog :one
UPDATE catalogs
SET name = $3, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: DeleteCatalog :execrows
DELETE FROM catalogs WHERE id = $1 AND store_id = $2;

-- name: AddCatalogProduct :exec
INSERT INTO catalog_products (catalog_id, product_id, position)
VALUES ($1, $2, $3)
ON CONFLICT (catalog_id, product_id) DO UPDATE SET position = EXCLUDED.position;

-- name: ClearCatalogProducts :exec
DELETE FROM catalog_products WHERE catalog_id = $1;

-- name: ListCatalogProducts :many
-- Products of a catalog, in display order. Scoped by store_id for safety.
SELECT p.id, p.name, p.keyword, p.price, p.image_url, p.stock, p.active, cp.position
FROM catalog_products cp
JOIN products p ON p.id = cp.product_id
JOIN catalogs c ON c.id = cp.catalog_id
WHERE cp.catalog_id = $1 AND c.store_id = $2
ORDER BY cp.position ASC, p.name ASC;

-- name: SetLiveEventCatalog :execrows
-- Associate (or clear, when $1 is NULL) a catalog with an event. Store-scoped.
UPDATE live_events
SET catalog_id = $1, updated_at = now()
WHERE id = $2 AND store_id = $3;

-- name: GetLiveEventCatalog :one
-- The catalog currently associated with an event (store-scoped). Errors if none.
SELECT c.*
FROM live_events e
JOIN catalogs c ON c.id = e.catalog_id
WHERE e.id = $1 AND e.store_id = $2;

-- name: GetCatalogByEventPublic :one
-- Public (buyer-facing) lookup: the catalog for an event, no store scoping.
SELECT c.*
FROM live_events e
JOIN catalogs c ON c.id = e.catalog_id
WHERE e.id = $1;

-- name: ListCatalogProductsByEventPublic :many
-- Public (buyer-facing) products of the event's catalog, in display order.
-- Only active products are returned to buyers.
SELECT p.id, p.name, p.keyword, p.price, p.image_url, p.stock, cp.position
FROM live_events e
JOIN catalog_products cp ON cp.catalog_id = e.catalog_id
JOIN products p ON p.id = cp.product_id
WHERE e.id = $1 AND p.active = true
ORDER BY cp.position ASC, p.name ASC;
