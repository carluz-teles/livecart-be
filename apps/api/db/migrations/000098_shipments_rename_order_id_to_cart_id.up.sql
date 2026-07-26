-- Renomeia a coluna legada `shipments.order_id` → `cart_id`.
--
-- Apesar do nome "order_id", a coluna sempre guardou carts(id) (FK legada da
-- migration 000052) — nunca a identidade do pedido. A FK correta para o pedido
-- vive em `orders_order_id` (migration 000097). O nome antigo era enganoso e a
-- única razão de "funcionar" era o mundo pré-split, onde o id que circulava como
-- "order id" também era o cart id. O novo nome reflete a semântica real.
--
-- RENAME COLUMN é uma operação de catálogo (sem rewrite da tabela nem dos dados);
-- índices e FKs seguem a coluna automaticamente. `orders_order_id` NÃO muda.
ALTER TABLE shipments RENAME COLUMN order_id TO cart_id;
ALTER INDEX shipments_order_id_idx RENAME TO shipments_cart_id_idx;

COMMENT ON COLUMN shipments.cart_id IS
    'FK para carts(id) (migration 000052, antes chamada order_id). Guarda o cart id de origem — NÃO a identidade do pedido, que vive em orders_order_id (migration 000097). Consumida pelos hooks de postcheckout que ainda tratam o valor como cart id.';
