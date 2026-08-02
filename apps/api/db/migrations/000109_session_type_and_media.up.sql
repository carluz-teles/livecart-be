-- D3 + D1 (adendo A4) — o TIPO e a MÍDIA descem do evento para a sessão/mídia.
--
-- Por que os dois JUNTOS: as queries do pipeline de post/story filtram
-- (media_id, type, status) na MESMA linha de live_events. Descer só o `type`
-- deixaria polling, webhook_active, fechamento por mídia apagada e sweep de
-- ends_at resolvendo por chaves em tabelas diferentes — inconsistentes entre si
-- no primeiro evento com mais de uma mídia.
--
-- EXPAND: nada é removido de live_events. type e media_* continuam existindo e
-- sendo escritos (dual-write) para o rollback continuar possível e para o FE,
-- que ainda lê event.type, não quebrar de uma vez. O DROP é a 000119 (contract).

SET lock_timeout = '5s';

-- 1. Tipo na sessão. 'reel' é valor NOVO: hoje um Reel publicado pelo LiveCart é
--    gravado como 'post' e fica indistinguível de um post de feed.
ALTER TABLE live_sessions
    ADD COLUMN IF NOT EXISTS type VARCHAR NOT NULL DEFAULT 'live';

-- Conversão do vocabulário do evento para o da sessão. 'single' e 'multi'
-- descrevem quantas plataformas a LIVE usava — as duas são live.
-- Não há reel histórico para converter: live_events.type nunca teve esse valor.
UPDATE live_sessions ls
SET type = CASE e.type
        WHEN 'post'  THEN 'post'
        WHEN 'story' THEN 'story'
        ELSE 'live'
    END
FROM live_events e
WHERE e.id = ls.event_id
  AND ls.type = 'live';

-- CHECK que live_events.type nunca teve (a 000021 criou a coluna sem nenhum).
ALTER TABLE live_sessions DROP CONSTRAINT IF EXISTS live_sessions_type_check;
ALTER TABLE live_sessions ADD CONSTRAINT live_sessions_type_check
    CHECK (type IN ('live', 'post', 'reel', 'story'));

CREATE INDEX IF NOT EXISTS idx_live_sessions_type ON live_sessions (type);

COMMENT ON COLUMN live_sessions.type IS
    'D3: natureza da transmissão (live|post|reel|story). Fonte da verdade do tipo. live_events.type sobrevive como rótulo legado do vocabulário antigo (single|multi|post|story) até a 000119; ''reel'' é gravado lá como ''post'' para não quebrar o FE.';

-- 2. Metadados da publicação descem do EVENTO para a MÍDIA.
--    Origem: 000076_post_events.up.sql:6-12.
--    O media_id não ganha coluna nova: seu destino sempre foi o
--    live_session_platforms.platform_live_id (a própria 000076 diz isso no
--    cabeçalho) e é assim que CreatePostEvent já grava hoje.
ALTER TABLE live_session_platforms
    ADD COLUMN IF NOT EXISTS media_permalink     VARCHAR,
    ADD COLUMN IF NOT EXISTS media_thumbnail_url VARCHAR,
    ADD COLUMN IF NOT EXISTS media_caption       TEXT,
    ADD COLUMN IF NOT EXISTS webhook_active      BOOLEAN NOT NULL DEFAULT false;

-- Casamento por platform_live_id = live_events.media_id. Não é reparo de dado
-- faltante (a base transacional é zerada antes do épico) — é só evitar que uma
-- base de desenvolvimento/staging perca a legenda e o permalink no cutover.
UPDATE live_session_platforms lsp
SET media_permalink     = e.media_permalink,
    media_thumbnail_url = e.media_thumbnail_url,
    media_caption       = e.media_caption,
    webhook_active      = e.webhook_active
FROM live_sessions ls
JOIN live_events e ON e.id = ls.event_id
WHERE ls.id = lsp.session_id
  AND e.media_id IS NOT NULL
  AND e.media_id = lsp.platform_live_id;

-- O polling percorre as mídias que ainda não receberam webhook.
CREATE INDEX IF NOT EXISTS idx_lsp_webhook_pending
    ON live_session_platforms (platform_live_id)
    WHERE webhook_active = false;

COMMENT ON COLUMN live_session_platforms.webhook_active IS
    'D1/D3: true quando um webhook de comments já chegou para ESTA mídia; o polling daquela mídia para. Substitui live_events.webhook_active, que desligava o polling do EVENTO inteiro — o que cegaria a segunda mídia de um evento guarda-chuva.';
