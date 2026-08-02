-- Reverte o indice. As duplicatas colapsadas no passo 1 NAO voltam: foram
-- marcadas 'cancelled' e nao ha como distingui-las de um cancelamento real do
-- comprador. Perda de dado aceita e declarada.
DROP INDEX IF EXISTS uq_waitlist_live_entry;
