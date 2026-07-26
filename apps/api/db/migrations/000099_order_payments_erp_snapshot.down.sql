-- Reverte a Fatia 11b: remove a coluna de snapshot (o backfill das demais
-- colunas não é revertido — elas já existiam e a fonte cart segue intacta).
ALTER TABLE order_payments DROP COLUMN erp_payment_snapshot;
