-- Remove a atribuição por sessão.
--
-- ATENÇÃO: isto NÃO recolapsa as linhas. Um pedido selado depois da 000107 pode
-- ter duas linhas do mesmo produto (uma por sessão); sem session_id elas viram
-- duas linhas idênticas do mesmo produto, o que a UI de pedido não espera.
--
-- Para descobrir o estrago antes de reverter:
--   SELECT order_id, product_id, count(*) FROM order_items
--   GROUP BY 1,2 HAVING count(*) > 1;
--
-- Recolapsar é decisão de negócio (somar quantidades com preços diferentes exige
-- escolher qual preço fica), não de migration.

DROP INDEX IF EXISTS idx_order_items_session;

ALTER TABLE order_items DROP COLUMN IF EXISTS session_id;

COMMENT ON TABLE order_items IS NULL;
