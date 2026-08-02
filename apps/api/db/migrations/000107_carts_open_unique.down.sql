-- Volta para a constraint TOTAL.
--
-- ATENÇÃO: se o modelo novo já tiver criado um segundo carrinho para algum
-- (evento, comprador) — que é exatamente o que a RN-07 e a RN-08 passam a
-- permitir —, este ADD CONSTRAINT FALHA. Não é bug do down: é o dado novo não
-- cabendo no schema antigo.
--
-- Para descobrir antes de tentar:
--   SELECT event_id, platform_user_id, count(*) FROM carts
--   GROUP BY 1,2 HAVING count(*) > 1;
--
-- Reverter de verdade exige decidir qual carrinho de cada par sobrevive — e isso
-- é decisão de negócio (o pago? o mais recente?), não de migration.

DROP INDEX IF EXISTS carts_one_open_per_event_buyer;

ALTER TABLE carts ADD CONSTRAINT carts_session_id_platform_user_id_key
    UNIQUE (event_id, platform_user_id);
