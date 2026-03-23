-- name: CreateProduct :one
INSERT INTO products (store_id, name, external_id, external_source, keyword, price, image_url, sizes, stock)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetProductByID :one
SELECT * FROM products WHERE id = $1 AND store_id = $2;

-- name: GetProductByKeyword :one
SELECT * FROM products WHERE store_id = $1 AND keyword = $2 AND active = true;

-- name: ListProductsByStore :many
SELECT * FROM products WHERE store_id = $1 ORDER BY created_at DESC;

-- name: UpdateProduct :one
UPDATE products
SET name = $3, price = $4, image_url = $5, sizes = $6, stock = $7, active = $8, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: GetMaxKeyword :one
SELECT COALESCE(MAX(keyword), '0999') AS max_keyword
FROM products
WHERE store_id = $1;
