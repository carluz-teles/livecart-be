-- name: CreateProduct :one
INSERT INTO products (
    store_id, name, external_id, external_source, keyword, price, image_url, stock,
    weight_grams, height_cm, width_cm, length_cm, sku, package_format, insurance_value_cents,
    group_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: SetProductGroup :exec
UPDATE products SET group_id = $2, updated_at = now() WHERE id = $1;

-- name: ListProductsByGroup :many
SELECT * FROM products WHERE group_id = $1 ORDER BY keyword ASC;

-- name: GetProductByID :one
SELECT * FROM products WHERE id = $1 AND store_id = $2;

-- name: GetProductByKeyword :one
SELECT * FROM products WHERE store_id = $1 AND keyword = $2 AND active = true;

-- name: GetProductByExternalID :one
-- Resolve um produto local pelo identificador do ERP. Usado pelo webhook
-- Tiny para mapear o produto antes de processar a fila de waitlist.
SELECT * FROM products
WHERE store_id = $1 AND external_source = $2 AND external_id = $3;

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

-- name: DecrementProductStockUpTo :one
-- Toma ATÉ `want` unidades, nunca abaixo de zero, e retorna quantas foram
-- de fato tomadas (0 quando o produto já estava esgotado). Habilita a
-- promoção PARCIAL da waitlist: 1 unidade livre atende parte de um pedido de
-- N na fila; o restante continua esperando. FOR UPDATE serializa contra
-- liberações concorrentes.
WITH before AS (
    SELECT stock AS s FROM products WHERE id = sqlc.arg(id) FOR UPDATE
)
UPDATE products p
SET stock = p.stock - LEAST(before.s, sqlc.arg(want)::int),
    updated_at = now()
FROM before
WHERE p.id = sqlc.arg(id)
RETURNING LEAST(before.s, sqlc.arg(want)::int)::int AS taken;
