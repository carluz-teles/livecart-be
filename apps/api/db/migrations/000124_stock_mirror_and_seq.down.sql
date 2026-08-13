-- Sem o seq, a escrita do saldo do ERP volta a ser incondicional.
ALTER TABLE products DROP COLUMN IF EXISTS erp_seq;
