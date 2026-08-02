-- D5 + D21 — separa a JANELA COMERCIAL do evento (starts_at/ends_at) do
-- AGENDAMENTO DE PUBLICACAO da sessao (publish_at).
--
-- Hoje live_events.scheduled_at acumula os dois sentidos, e post/story nascem
-- com status='active' + scheduled_at futuro de proposito, porque a query de
-- resolucao do webhook exige status='active' (workaround documentado em
-- internal/live/service.go, CreatePostEvent).
--
-- Por que ends_at NAO recebe NOT NULL nem CHECK aqui: em Postgres um
-- CHECK ... NOT VALID e aplicado em QUALQUER UPDATE de linha existente, nao so
-- em INSERT. Um CHECK (ends_at IS NOT NULL) faria todo UPDATE em evento legado
-- sem ends_at falhar — inclusive IncrementLiveEventOrders, que roda no caminho
-- de PAGAMENTO. A obrigatoriedade fica na camada de aplicacao (validacao no
-- create/update) e a constraint de banco so entra na 000119.

SET lock_timeout = '5s';

-- ---------------------------------------------------------------------------
-- 1. Janela comercial do evento.
-- ---------------------------------------------------------------------------
ALTER TABLE live_events ADD COLUMN IF NOT EXISTS starts_at TIMESTAMPTZ;

-- Backfill conservador: so copia onde ja existe intencao explicita de inicio.
-- Evento historico sem scheduled_at fica com starts_at NULL — inventar uma data
-- mudaria rotulo ja visto pelo lojista.
UPDATE live_events
SET starts_at = scheduled_at
WHERE starts_at IS NULL AND scheduled_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_live_events_starts_at
    ON live_events (starts_at) WHERE starts_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. ends_at para eventos JA ENCERRADOS. Seguro por construcao: o evento ja
--    acabou, entao gravar uma data no passado nao pode disparar fechamento
--    nenhum. NAO tocamos em evento vivo — gravar ends_at num evento ativo o
--    FECHARIA no primeiro sweep.
-- ---------------------------------------------------------------------------
UPDATE live_events e
SET ends_at = COALESCE(
        (SELECT MAX(ls.ended_at) FROM live_sessions ls WHERE ls.event_id = e.id),
        e.updated_at)
WHERE e.status = 'ended' AND e.ends_at IS NULL;

COMMENT ON COLUMN live_events.starts_at IS
    'D21: inicio da JANELA COMERCIAL da campanha. Nao e a data de publicacao da midia (essa e live_sessions.publish_at). Mantido em sincronia com scheduled_at ate a 000119.';
COMMENT ON COLUMN live_events.ends_at IS
    'D5: TETO da campanha. Obrigatorio para eventos NOVOS (validado na aplicacao). A obrigatoriedade no banco entra na 000119, depois do backfill dos legados.';

-- ---------------------------------------------------------------------------
-- 3. Agendamento de PUBLICACAO, na sessao.
-- ---------------------------------------------------------------------------
ALTER TABLE live_sessions ADD COLUMN IF NOT EXISTS publish_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_live_sessions_publish_at
    ON live_sessions (publish_at) WHERE publish_at IS NOT NULL;

COMMENT ON COLUMN live_sessions.publish_at IS
    'D21: quando o LiveCart deve publicar a midia desta sessao no Instagram. Sessao de live NAO tem publish_at (nao se publica live por API). Antes de publish_at nao existe midia, logo nao existe comentario — e o que remove a ambiguidade do webhook. O agendador em si e a 000120.';
