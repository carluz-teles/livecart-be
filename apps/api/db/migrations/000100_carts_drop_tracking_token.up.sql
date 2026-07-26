-- Fatia 10-a: a fonte da verdade do tracking_token é order_logistics (Fatia C1,
-- migration 000094, com idx_order_logistics_tracking garantindo unicidade global).
-- O lookup público de rastreio e o postcheckout já resolvem/gravam o token só na
-- Order (order_logistics), então a cópia pós-venda no cart pode sair. O cart segue
-- sendo a "order em draft" (pré-pagamento); apenas o dado pós-venda migra.

DROP INDEX IF EXISTS idx_carts_tracking_token;

ALTER TABLE carts DROP COLUMN IF EXISTS tracking_token;
