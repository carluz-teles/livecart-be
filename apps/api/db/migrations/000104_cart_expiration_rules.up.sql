-- Regras de prazo do carrinho para o evento guarda-chuva (RN-34, RN-35).
--
-- Duas mudanças que precisam vir juntas porque mexem na mesma configuração:
--
-- 1. O valor 0 ("sem expiração") deixa de existir; piso de 15 minutos.
--
--    Hoje FinalizeCartsByEvent PRESERVA o expires_at existente quando o prazo
--    efetivo é 0. No modelo novo o carrinho fica com expires_at NULL durante o
--    evento inteiro — por definição, não por falha. Então 0 deixa de significar
--    "sem prazo" e passa a significar "carrinho eterno", exatamente no momento
--    do fechamento: nenhuma task cart.expire é agendada e não existe mais sweep
--    de carrinhos para alcançá-lo.
--
--    A conversão 0 -> 1440 (24h) muda o prazo dessas lojas. A lista precisa ter
--    sido levantada e os lojistas avisados ANTES desta migration:
--      SELECT id, name FROM stores WHERE cart_expiration_minutes = 0;
--      SELECT count(*) FROM live_events WHERE cart_expiration_minutes = 0;
--
-- 2. Nasce o prazo ESTENDIDO, usado quando close_cart_on_event_end = false.
--
--    O toggle deixa de ser "ter prazo x não ter prazo" e passa a escolher qual
--    dos dois prazos vale. Ligado: prazo curto (o cart_expiration_minutes que
--    já existe). Desligado: prazo estendido, para recuperação de venda. Os dois
--    ramos armam cart.expire pelo mesmo mecanismo — nada fica eterno e nenhum
--    sweep de carrinhos precisa voltar.

-- ---------------------------------------------------------------------------
-- 1. Piso na loja. 0 era "sem expiração" e vira 24h.
-- ---------------------------------------------------------------------------
UPDATE stores SET cart_expiration_minutes = 1440
WHERE cart_expiration_minutes = 0;

-- Valores legados entre 1 e 14 não deveriam existir (a UI nunca ofereceu), mas
-- o CHECK abaixo falharia com eles e derrubaria a migration inteira.
UPDATE stores SET cart_expiration_minutes = 15
WHERE cart_expiration_minutes BETWEEN 1 AND 14;

ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_cart_expiration_minutes_check;
ALTER TABLE stores ADD CONSTRAINT stores_cart_expiration_minutes_check
    CHECK (cart_expiration_minutes >= 15);

COMMENT ON COLUMN stores.cart_expiration_minutes IS
    'Minutos de prazo do carrinho depois de armado, quando close_cart_on_event_end = true. Mínimo 15. O valor 0 ("sem expiração") foi REMOVIDO — produzia carrinho eterno no fechamento do evento.';

-- ---------------------------------------------------------------------------
-- 2. Mesmo piso no override por evento. É a mesma coluna e alimenta o mesmo
--    COALESCE, então um 0 aqui reproduz o bug mesmo com a loja já corrigida.
--    NULL continua válido e significa "herda da loja".
-- ---------------------------------------------------------------------------
UPDATE live_events SET cart_expiration_minutes = 1440
WHERE cart_expiration_minutes = 0;

UPDATE live_events SET cart_expiration_minutes = 15
WHERE cart_expiration_minutes BETWEEN 1 AND 14;

ALTER TABLE live_events DROP CONSTRAINT IF EXISTS live_events_cart_expiration_minutes_check;
ALTER TABLE live_events ADD CONSTRAINT live_events_cart_expiration_minutes_check
    CHECK (cart_expiration_minutes IS NULL OR cart_expiration_minutes >= 15);

COMMENT ON COLUMN live_events.cart_expiration_minutes IS
    'Override do prazo curto do carrinho para este evento. NULL = herda de stores.cart_expiration_minutes. Mínimo 15 — o valor 0 foi removido.';

-- ---------------------------------------------------------------------------
-- 3. Prazo estendido (ramo desligado do close_cart_on_event_end).
--    Na loja é NOT NULL porque é o fallback; no evento é nulável, seguindo o
--    mesmo padrão da coluna de prazo curto. Default 10080 min = 7 dias.
-- ---------------------------------------------------------------------------
ALTER TABLE stores
    ADD COLUMN IF NOT EXISTS cart_extended_expiration_minutes INTEGER NOT NULL DEFAULT 10080;

ALTER TABLE stores DROP CONSTRAINT IF EXISTS stores_cart_extended_expiration_check;
ALTER TABLE stores ADD CONSTRAINT stores_cart_extended_expiration_check
    CHECK (cart_extended_expiration_minutes >= 15);

COMMENT ON COLUMN stores.cart_extended_expiration_minutes IS
    'Prazo do carrinho quando close_cart_on_event_end = FALSE. Default 10080 (7 dias). O ramo desligado NÃO é "sem prazo" — é um prazo maior, armado pelo mesmo cart.expire do ramo ligado.';

ALTER TABLE live_events
    ADD COLUMN IF NOT EXISTS cart_extended_expiration_minutes INTEGER;

ALTER TABLE live_events DROP CONSTRAINT IF EXISTS live_events_cart_extended_expiration_check;
ALTER TABLE live_events ADD CONSTRAINT live_events_cart_extended_expiration_check
    CHECK (cart_extended_expiration_minutes IS NULL OR cart_extended_expiration_minutes >= 15);

COMMENT ON COLUMN live_events.cart_extended_expiration_minutes IS
    'Override do prazo estendido para este evento. NULL = herda de stores.cart_extended_expiration_minutes.';
