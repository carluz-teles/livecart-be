-- name: CreateProduct :one
INSERT INTO products (
    store_id, name, external_id, external_source, keyword, price, image_url, stock,
    weight_grams, height_cm, width_cm, length_cm, sku, package_format, insurance_value_cents
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING *;

-- name: GetProductByID :one
SELECT * FROM products WHERE id = $1 AND store_id = $2;

-- name: GetProductByKeyword :one
SELECT * FROM products WHERE store_id = $1 AND keyword = $2 AND active = true;

-- name: ListProductsByStore :many
SELECT * FROM products WHERE store_id = $1 ORDER BY created_at DESC;

-- name: UpdateProduct :one
UPDATE products
SET name = $3,
    price = $4,
    image_url = $5,
    stock = $6,
    active = $7,
    weight_grams = $8,
    height_cm = $9,
    width_cm = $10,
    length_cm = $11,
    sku = $12,
    package_format = $13,
    insurance_value_cents = $14,
    updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: GetMaxKeyword :one
SELECT COALESCE(MAX(keyword), '0999') AS max_keyword
FROM products
WHERE store_id = $1;

-- name: DecrementProductStock :one
-- Atomically decrement stock. Fails (no rows) if insufficient stock.
UPDATE products
SET stock = stock - $2, updated_at = now()
WHERE id = $1 AND stock >= $2
RETURNING *;

-- name: IncrementProductStock :one
-- Release reserved stock back to product.
UPDATE products
SET stock = stock + $2, updated_at = now()
WHERE id = $1
RETURNING *;
