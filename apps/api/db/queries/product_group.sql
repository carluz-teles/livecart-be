-- name: CreateProductGroup :one
INSERT INTO product_groups (store_id, name, description, external_id, external_source)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetProductGroupByID :one
SELECT * FROM product_groups WHERE id = $1 AND store_id = $2;

-- name: GetProductGroupByExternalID :one
SELECT * FROM product_groups
WHERE store_id = $1 AND external_source = $2 AND external_id = $3;

-- name: ListProductGroupsByStore :many
SELECT g.*, COUNT(p.id)::INT AS variants_count
FROM product_groups g
LEFT JOIN products p ON p.group_id = g.id
WHERE g.store_id = $1
GROUP BY g.id
ORDER BY g.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountProductGroupsByStore :one
SELECT COUNT(*)::INT FROM product_groups WHERE store_id = $1;

-- name: UpdateProductGroup :one
UPDATE product_groups
SET name = $3, description = $4, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: DeleteProductGroup :exec
DELETE FROM product_groups WHERE id = $1 AND store_id = $2;

-- name: CreateProductOption :one
INSERT INTO product_options (group_id, name, position)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListProductOptionsByGroup :many
SELECT * FROM product_options WHERE group_id = $1 ORDER BY position ASC, name ASC;

-- name: DeleteProductOptionsByGroup :exec
DELETE FROM product_options WHERE group_id = $1;

-- name: CreateProductOptionValue :one
INSERT INTO product_option_values (option_id, value, position)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListProductOptionValuesByOption :many
SELECT * FROM product_option_values WHERE option_id = $1 ORDER BY position ASC, value ASC;

-- name: ListProductOptionValuesByGroup :many
SELECT v.id, v.option_id, v.value, v.position, o.name AS option_name, o.position AS option_position
FROM product_option_values v
JOIN product_options o ON o.id = v.option_id
WHERE o.group_id = $1
ORDER BY o.position ASC, v.position ASC;

-- name: AssignVariantOption :exec
INSERT INTO product_variant_options (product_id, option_value_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: ListVariantOptionsByProduct :many
SELECT v.id, v.option_id, v.value, v.position, o.name AS option_name, o.position AS option_position
FROM product_variant_options pvo
JOIN product_option_values v ON v.id = pvo.option_value_id
JOIN product_options o ON o.id = v.option_id
WHERE pvo.product_id = $1
ORDER BY o.position ASC;

-- name: ListVariantOptionsByGroup :many
-- Returns one row per (product_id, option_value) for every variant in a group.
-- Used to denormalize ProductResponse.optionValues for listing endpoints.
SELECT pvo.product_id, v.id AS option_value_id, v.value, o.name AS option_name, o.position AS option_position
FROM product_variant_options pvo
JOIN product_option_values v ON v.id = pvo.option_value_id
JOIN product_options o ON o.id = v.option_id
JOIN products p ON p.id = pvo.product_id
WHERE p.group_id = $1
ORDER BY pvo.product_id, o.position ASC;

-- name: DeleteVariantOptionsByProduct :exec
DELETE FROM product_variant_options WHERE product_id = $1;
