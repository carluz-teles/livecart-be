-- name: CreateProductImage :one
INSERT INTO product_images (product_id, url, position)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListProductImagesByProduct :many
SELECT * FROM product_images WHERE product_id = $1 ORDER BY position ASC, created_at ASC;

-- name: ListProductImagesByGroup :many
SELECT pi.* FROM product_images pi
JOIN products p ON p.id = pi.product_id
WHERE p.group_id = $1
ORDER BY pi.product_id, pi.position ASC;

-- name: DeleteProductImage :exec
DELETE FROM product_images WHERE id = $1 AND product_id = $2;

-- name: DeleteProductImagesByProduct :exec
DELETE FROM product_images WHERE product_id = $1;

-- name: CreateProductGroupImage :one
INSERT INTO product_group_images (group_id, url, position)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListProductGroupImagesByGroup :many
SELECT * FROM product_group_images WHERE group_id = $1 ORDER BY position ASC, created_at ASC;

-- name: DeleteProductGroupImage :exec
DELETE FROM product_group_images WHERE id = $1 AND group_id = $2;

-- name: DeleteProductGroupImagesByGroup :exec
DELETE FROM product_group_images WHERE group_id = $1;
