-- Código de barras (GTIN/EAN) do produto.
--
-- O Tiny já devolve `gtin` em `GET /produtos/{id}`, tanto do produto quanto de
-- cada variação, e o parser já o lia — o valor era descartado por não ter onde
-- morar. Sem ele, a busca interna do catálogo não acha um produto pelo código
-- que o lojista tem à mão (na etiqueta, no leitor, na nota).
--
-- Índice parcial no mesmo formato do de SKU: a busca é sempre dentro de UMA
-- loja, e produto sem código de barras não ocupa espaço no índice.
ALTER TABLE products
    ADD COLUMN IF NOT EXISTS barcode VARCHAR;

COMMENT ON COLUMN products.barcode IS 'Barcode (GTIN/EAN) as registered in the ERP. Used by the catalog search alongside sku and keyword.';

CREATE INDEX IF NOT EXISTS idx_products_barcode
    ON products (store_id, barcode)
    WHERE barcode IS NOT NULL;
