-- D23 — as duas FKs que orders nunca teve. orders.event_id e orders.store_id
-- sao UUID soltos desde a 000094 (linhas 5 e 6): nada no banco garante que
-- apontam para linha existente.
--
-- PREMISSA CORRIGIDA, e ela ENCOLHE o escopo. A decisao 23 dizia "hard delete
-- com CASCADE deixa orders orfas". Nao deixa: orders.cart_id referencia
-- carts(id) SEM clausula ON DELETE (000094), portanto NO ACTION, e isso ja
-- BLOQUEIA o CASCADE de live_events -> carts. Hoje apagar um evento com
-- qualquer pedido ja falha — so que falha como violacao de FK em cima de uma
-- tabela que o lojista nunca ouviu falar, virando 500 sem mensagem util
-- (live/repository.go, DELETE cru via pool.Exec).
--
-- Ou seja: o ganho aqui NAO e integridade (ela ja existia por tabela; o que
-- faltava era a declaracao). O ganho e (a) a regra ficar escrita onde se
-- procura por ela e (b) a violacao passar a apontar para live_events, que e o
-- objeto do qual o lojista fala.
--
-- ON DELETE RESTRICT e nao NO ACTION de proposito: RESTRICT e checado na hora,
-- nao no fim da transacao. Numa transacao que apaga evento e pedido junto,
-- NO ACTION deixaria a exclusao "quase passar" e so estourar no COMMIT.
--
-- NOT VALID: cria a regra valendo para linha NOVA sem varrer a tabela agora.
-- ADD CONSTRAINT NOT VALID pega SHARE ROW EXCLUSIVE, que e curto; a varredura
-- (VALIDATE) fica na 000120, em transacao propria e com lock fraco. Fazer as
-- duas no MESMO arquivo nao daria ganho nenhum de lock, porque o
-- golang-migrate roda o arquivo inteiro numa transacao so.

SET lock_timeout = '5s';

ALTER TABLE orders
    ADD CONSTRAINT orders_event_id_fkey
    FOREIGN KEY (event_id) REFERENCES live_events(id) ON DELETE RESTRICT NOT VALID;

ALTER TABLE orders
    ADD CONSTRAINT orders_store_id_fkey
    FOREIGN KEY (store_id) REFERENCES stores(id) ON DELETE RESTRICT NOT VALID;

COMMENT ON CONSTRAINT orders_event_id_fkey ON orders IS
    'D23: pedido pertence a um evento existente. ON DELETE RESTRICT — evento com venda nao se apaga, se encerra. A mensagem de negocio e do handler; esta constraint e a rede embaixo.';

COMMENT ON CONSTRAINT orders_store_id_fkey ON orders IS
    'D23: pedido pertence a uma loja existente. Mesmo racional do orders_event_id_fkey.';
