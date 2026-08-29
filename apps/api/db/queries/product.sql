-- name: CreateProduct :one
-- O id vem do DOMÍNIO, não do banco.
--
-- Sem ele na lista, o Postgres gerava um id próprio e o objeto em memória ficava
-- com outro — e quem confiasse no retorno de Save/ImportProduct recebia um id que
-- não corresponde a linha nenhuma. O import manual da tela devolvia esse id, e o
-- reflexo do pedido no ERP o usaria para amarrar o item ao carrinho.
INSERT INTO products (
    id, store_id, name, external_id, external_source, keyword, price, image_url, stock,
    weight_grams, height_cm, width_cm, length_cm, sku, package_format, insurance_value_cents,
    group_id, barcode
)
VALUES (sqlc.arg(id), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
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
    barcode = $15,
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
SET stock = stock - $2, erp_seq = erp_seq + 1, updated_at = now()
WHERE id = $1 AND stock >= $2
RETURNING *;

-- name: IncrementProductStock :one
-- Release reserved stock back to product.
UPDATE products
SET stock = stock + $2, erp_seq = erp_seq + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ForceDecrementProductStock :one
-- Retoma estoque SEM piso em zero. Único caso de uso: o cart cancelado pelo
-- lojista que acabou pago (RestoreCancelledCartAsPaid) — a unidade já foi
-- vendida de verdade, então o saldo tem de refletir isso mesmo que fique
-- negativo. Saldo negativo é o sinal honesto de "vendi mais do que tinha" e
-- aparece para o lojista; recusar o decremento esconderia a venda.
-- NÃO usar em fluxo de compra normal — lá o piso do DecrementProductStock é
-- justamente o que impede overselling.
UPDATE products
SET stock = stock - $2, erp_seq = erp_seq + 1, updated_at = now()
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
    erp_seq = p.erp_seq + 1,
    updated_at = now()
FROM before
WHERE p.id = sqlc.arg(id)
RETURNING LEAST(before.s, sqlc.arg(want)::int)::int AS taken;


-- name: ProductSeqByExternalID :one
-- Resolve o produto local pelo codigo do ERP e devolve o seq do mesmo instante.
--
-- Uma consulta so porque as duas informacoes tem de vir juntas: o id para saber
-- onde escrever, e o seq para saber se a escrita ainda valera.
SELECT id, erp_seq FROM products
WHERE store_id = sqlc.arg(store_id)
  AND external_source = sqlc.arg(external_source)
  AND external_id = sqlc.arg(external_id)
LIMIT 1;

-- name: ApplyERPStockMirror :execrows
-- Escreve o saldo lido do ERP, e SO se nenhum movimento nosso tiver acontecido
-- desde a leitura.
--
-- Esta e a trava que substitui as heuristicas de janela. O webhook do Tiny nao
-- traz timestamp nem sequencia, entao nao ha como ordenar dois saldos pelo
-- conteudo. O seq resolve pelo unico lado que controlamos: os nossos proprios
-- movimentos, cada um subindo o contador quando altera o estoque.
--
-- Zero linhas significa "leitura vencida", e o certo e descartar — nao aplicar
-- com ressalva, porque nao ha como saber quanto daquele numero ja estava velho.
-- Uma leitura nova vem no proximo webhook ou na reconciliacao.
--
-- Saldo negativo do ERP nao e estoque, e sim sintoma: o Tiny aceita ir abaixo de
-- zero (gravado na bateria de sandbox) e copiar isso propagaria o defeito.
UPDATE products
SET stock = GREATEST(sqlc.arg(erp_stock)::int, 0), updated_at = now()
WHERE id = sqlc.arg(id) AND erp_seq = sqlc.arg(seen_seq)::bigint;

-- name: ListERPLinkedProductsSample :many
-- Uma amostra pequena de produtos ligados ao ERP, dos que TÊM estoque — são os
-- únicos em que uma reserva poderia aparecer.
--
-- Serve à checagem do módulo de Reserva de Estoque, e a amostra é pequena de
-- propósito: as leituras dividem a cota da conta com a live, e provar um módulo
-- não vale atrasar uma venda.
SELECT id, name, external_id
FROM products
WHERE store_id = sqlc.arg(store_id)::uuid
  AND external_source = 'tiny'
  AND external_id IS NOT NULL AND external_id <> ''
  AND stock > 0
ORDER BY updated_at DESC NULLS LAST
LIMIT sqlc.arg(limite)::int;
