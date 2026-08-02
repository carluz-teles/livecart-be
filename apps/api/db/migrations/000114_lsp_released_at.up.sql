-- D22 / A5, fase 1 de 2 — "uma midia por evento ATIVO por vez".
--
-- A decisao 22 fala em "trocar a unique". NAO E. O predicado precisaria olhar
-- live_events.status, que esta a DUAS tabelas de distancia: live_session_platforms
-- so tem (id, session_id, platform, platform_live_id, added_at) e indice parcial
-- enxerga apenas colunas da PROPRIA linha. Saida: denormalizar o status do evento
-- numa coluna da midia. released_at IS NULL = "esta midia pertence a um evento
-- vivo". So com ela a regra vira um indice parcial legitimo (000115).
--
-- Quem mantem a coluna e um TRIGGER, nao o codigo Go. Deixar isso no EndLiveEvent
-- significaria que qualquer OUTRO caminho de encerramento — o sweep de ends_at,
-- EndEventByMediaID (que e SQL cru, fora do sqlc), um UPDATE manual num incidente —
-- deixaria a midia presa PARA SEMPRE, sem erro nenhum. E "presa" e indistinguivel
-- de "em uso" para o indice: o sintoma seria "o reuso de midia nao funciona".
--
-- Por que em duas fases: o trigger precisa estar NO AR e a coluna coerente ANTES
-- de a unique global cair. Invertendo, toda midia de evento encerrado DEPOIS da
-- troca nasceria com released_at NULL e o reuso simplesmente nao aconteceria.
-- Esta migration nao muda comportamento de resolucao nenhum.

SET lock_timeout = '5s';

ALTER TABLE live_session_platforms
    ADD COLUMN IF NOT EXISTS released_at TIMESTAMPTZ;

-- Backfill: toda midia de evento JA ENCERRADO esta liberada. ended_at da sessao
-- e o carimbo mais preciso; updated_at do evento e o ultimo recurso.
UPDATE live_session_platforms lsp
SET released_at = COALESCE(ls.ended_at, e.updated_at, now())
FROM live_sessions ls
JOIN live_events e ON e.id = ls.event_id
WHERE ls.id = lsp.session_id
  AND e.status = 'ended'
  AND lsp.released_at IS NULL;

-- ---------------------------------------------------------------------------
-- O trigger de UPDATE: released_at vira FUNCAO de live_events.status, mantida
-- pelo proprio banco, valendo para todo caminho de encerramento existente e
-- futuro.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION sync_lsp_released_at() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.status = 'ended' AND OLD.status IS DISTINCT FROM 'ended' THEN
        -- Campanha encerrou: libera as midias de todas as suas sessoes.
        UPDATE live_session_platforms lsp
        SET released_at = now()
        FROM live_sessions ls
        WHERE ls.id = lsp.session_id
          AND ls.event_id = NEW.id
          AND lsp.released_at IS NULL;

    ELSIF OLD.status = 'ended' AND NEW.status IS DISTINCT FROM 'ended' THEN
        -- Campanha reaberta: a midia volta a ficar em uso. Se outra campanha
        -- viva ja tomou a mesma midia, este UPDATE viola uq_lsp_media_in_flight
        -- (000115) e a reabertura FALHA — que e a resposta certa: duas campanhas
        -- vivas na mesma midia e exatamente o que a D22 proibe.
        UPDATE live_session_platforms lsp
        SET released_at = NULL
        FROM live_sessions ls
        WHERE ls.id = lsp.session_id
          AND ls.event_id = NEW.id
          AND lsp.released_at IS NOT NULL;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_sync_lsp_released_at ON live_events;
CREATE TRIGGER trg_sync_lsp_released_at
    AFTER UPDATE OF status ON live_events
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION sync_lsp_released_at();

COMMENT ON FUNCTION sync_lsp_released_at() IS
    'A5/D22: mantem live_session_platforms.released_at como denormalizacao de live_events.status. Um indice parcial nao enxerga colunas de outra tabela — esta funcao e o que torna "uma midia por evento ATIVO" expressavel. Toda transicao de status do evento passa por aqui, inclusive as feitas fora do caminho feliz (sweep, EndEventByMediaID em SQL cru, UPDATE manual).';

-- ---------------------------------------------------------------------------
-- O trigger de INSERT: midia vinculada DEPOIS do encerramento (reparo,
-- importacao) nao passa pelo trigger de UPDATE e nasceria "em uso".
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_lsp_released_at_on_insert() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.released_at IS NULL AND EXISTS (
        SELECT 1
        FROM live_sessions ls
        JOIN live_events e ON e.id = ls.event_id
        WHERE ls.id = NEW.session_id AND e.status = 'ended'
    ) THEN
        NEW.released_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_set_lsp_released_at_on_insert ON live_session_platforms;
CREATE TRIGGER trg_set_lsp_released_at_on_insert
    BEFORE INSERT ON live_session_platforms
    FOR EACH ROW
    EXECUTE FUNCTION set_lsp_released_at_on_insert();

-- O plano previa um idx_lsp_in_flight (parcial, WHERE released_at IS NULL) aqui,
-- dropado na 000115. Nao criamos: idx_live_session_platforms_platform_live_id ja
-- indexa a coluna inteira e continua necessario DEPOIS da 000115, porque a
-- resolucao passou a ler midia liberada tambem (D20). O parcial so faz sentido
-- como o UNIQUE da 000115 — criar e dropar na migration seguinte e churn.

COMMENT ON COLUMN live_session_platforms.released_at IS
    'D22: quando esta midia deixou de pertencer a um evento vivo. NULL = em uso. E a denormalizacao de live_events.status na linha da midia — necessaria porque um indice parcial nao enxerga colunas de outra tabela.';
