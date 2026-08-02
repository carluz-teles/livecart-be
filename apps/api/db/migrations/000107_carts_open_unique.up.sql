-- Um carrinho ABERTO por (evento, comprador) — RN-02.
--
-- Hoje a constraint é total: UNIQUE (event_id, platform_user_id), sem predicado.
-- O nome físico é legado — nasceu como UNIQUE (session_id, platform_user_id) na
-- 000001 e a 000020 renomeou a COLUNA, não a constraint. Verificado:
--   SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint
--   WHERE conrelid='carts'::regclass AND contype='u';
--   -> carts_session_id_platform_user_id_key | UNIQUE (event_id, platform_user_id)
--
-- Ela é a razão de o reopen ser destrutivo: como não dá para inserir um segundo
-- carrinho, o código apaga os itens do antigo e reabre no lugar
-- (live/repository.go:1196-1204 diz isso com todas as letras). Trocando por
-- índice PARCIAL, "pagou" e "expirou" liberam a vaga e o comprador ganha um
-- carrinho novo em vez de perder o que tinha (RN-07 e RN-08).
--
-- SOBRE O PREDICADO — é onde a primeira versão do plano errou.
--
-- Ele NÃO pode ser escrito sobre `status = 'paid'`: esse valor nunca existe.
-- O pagamento grava apenas payment_status (UpdateCartPayment, cart.sql:226-241);
-- o status permanece 'active' (pagou durante o evento) ou 'checkout' (pagou
-- depois). Um predicado sobre status='paid' não filtraria nada e o carrinho pago
-- continuaria bloqueando o segundo.
--
-- payment_status é NULLABLE (default 'pending'). Um `payment_status NOT IN (...)`
-- puro devolve NULL para essas linhas, que então ficam FORA do índice parcial —
-- e carrinhos duplicados passariam em silêncio. Por isso o IS NULL explícito.
--
-- A lista de status é POSITIVA de propósito. Com lista negativa, um status novo
-- criado no futuro passaria a ocupar a vaga e bloquearia a criação de carrinho —
-- falha que aparece como "o comprador não consegue comprar", em silêncio. Com
-- lista positiva, o pior caso é um carrinho a mais, que o guard de aplicação e
-- os testes pegam. 'pending' entra porque é o default da coluna: nenhum caminho
-- de produção o usa hoje (o único INSERT, cart.sql:6, fixa 'active'), mas se
-- algum passar a omitir o status, esse carrinho tem de ocupar a vaga.

ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_session_id_platform_user_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS carts_one_open_per_event_buyer
    ON carts (event_id, platform_user_id)
    WHERE status IN ('pending', 'active', 'checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'));

COMMENT ON INDEX carts_one_open_per_event_buyer IS
    'RN-02: no máximo UM carrinho aberto por (evento, comprador). Pago, estornado, cancelado e expirado liberam a vaga, permitindo um novo ciclo de compra na mesma campanha (RN-07/RN-08).';
