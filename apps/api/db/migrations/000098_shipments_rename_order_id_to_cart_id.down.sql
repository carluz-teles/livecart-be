-- Reverte o rename da migration 000098: cart_id → order_id (nome legado).
ALTER INDEX shipments_cart_id_idx RENAME TO shipments_order_id_idx;
ALTER TABLE shipments RENAME COLUMN cart_id TO order_id;

COMMENT ON COLUMN shipments.order_id IS
    'DEPRECATED: apesar do nome guarda carts(id) (FK legada da migration 000052). Use orders_order_id para a identidade do pedido. Remoção planejada em fatia futura.';
