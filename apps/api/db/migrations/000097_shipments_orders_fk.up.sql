-- Corrige o acoplamento CAMUFLADO da tabela `shipments` (introduzida na
-- migration 000052). A coluna se chama `order_id`, mas a FK aponta para
-- carts(id) e o valor guardado é o UUID do CART — não do pedido. Isso só
-- "funcionava" porque, antes do split Cart/Order, o id que circulava como
-- "order id" na API também era o cart id.
--
-- Após o split, a identidade correta de um pedido é orders(id). Introduzimos a
-- coluna `orders_order_id` com a FK CERTA para orders(id) e a populamos
-- resolvendo orders.cart_id = shipments.order_id (o cart id legado).
--
-- A coluna legada `order_id` (que, apesar do nome, guarda carts(id)) é MANTIDA
-- nesta fatia porque ainda há call sites que a usam como cart id (hooks de
-- postcheckout via OnShipmentPosted, ShipmentRow.OrderID). Renomear/remover é
-- um passo seguinte SEGURO só depois que todos esses call sites migrarem para
-- orders_order_id — fora do escopo desta fatia para não quebrar leituras.

ALTER TABLE shipments
    ADD COLUMN IF NOT EXISTS orders_order_id UUID REFERENCES orders(id) ON DELETE CASCADE;

-- Backfill: resolve o order id real a partir do cart id guardado em order_id.
-- Carts que nunca materializaram uma Order (não deveriam ter shipment) ficam
-- com orders_order_id NULL.
UPDATE shipments sh
SET    orders_order_id = o.id
FROM   orders o
WHERE  o.cart_id = sh.order_id
  AND  sh.orders_order_id IS NULL;

CREATE INDEX IF NOT EXISTS shipments_orders_order_id_idx ON shipments (orders_order_id);

COMMENT ON COLUMN shipments.orders_order_id IS
    'FK correta para orders(id). Substitui o uso camuflado de order_id (que guarda carts(id)). Populada via orders.cart_id = shipments.order_id.';
COMMENT ON COLUMN shipments.order_id IS
    'DEPRECATED: apesar do nome guarda carts(id) (FK legada da migration 000052). Use orders_order_id para a identidade do pedido. Remoção planejada em fatia futura.';
