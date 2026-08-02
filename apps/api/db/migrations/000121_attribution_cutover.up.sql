-- D26 + D12 — a migracao 1:1 e o MARCADOR DE CORTE da atribuicao por sessao.
--
-- Por que o marcador existe: depois deste epico, "receita da live de terca"
-- passa a ser calculada de um jeito DIFERENTE do de antes. Os numeros mudam.
-- Isso nao e bug — mas sem um registro consultavel, a primeira pessoa que
-- comparar os dois lados vai concluir que a metrica quebrou, e a feature perde
-- a confianca do lojista no primeiro dia.
--
-- ORDEM DURA: esta migration LE live_events.type (passo 1). Ela tem de rodar
-- ANTES da 000122, que dropa a coluna. Rodar depois falha com "column does not
-- exist" — e essa e a unica dependencia de ordem dura entre as migrations
-- novas, junto com a 000111/000114 que criaram as colunas usadas aqui.

-- =============================================================================
-- 1. Reparo 1:1 (D26): todo evento tem de ter pelo menos uma sessao.
-- =============================================================================
-- O esperado e ZERO linha inserida: todo caminho de criacao ja cria evento +
-- sessao na mesma transacao (live/repository.go, CreateEventWithSessionTx), e
-- o ramo que criava evento SEM sessao foi fechado antes desta fatia.
--
-- O reparo existe assim mesmo porque um evento sem sessao deixou de ser
-- inofensivo quando a whitelist (000112) e o modo live (000113) desceram para
-- live_sessions: sem sessao nao ha onde guardar configuracao, a whitelist
-- grava zero linhas e a leitura responde 404. Um evento legado nessa situacao
-- ficaria mudo para sempre.
--
-- sequence_order = 1 e seguro por construcao: so entram eventos sem NENHUMA
-- sessao, entao a unique (event_id, sequence_order) nao tem com quem colidir.
INSERT INTO live_sessions (event_id, status, sequence_order, type, started_at, ended_at, created_at, updated_at)
SELECT e.id,
       CASE WHEN e.status = 'ended' THEN 'ended' ELSE 'active' END,
       1,
       -- O vocabulario da sessao inclui 'reel', que live_events.type nunca
       -- teve: reels legados foram todos gravados como 'post' e sao
       -- indistinguiveis no dado. O backfill os mantem 'post' de proposito —
       -- reclassificar exigiria consultar a Graph API midia por midia.
       CASE e.type WHEN 'post' THEN 'post' WHEN 'story' THEN 'story' ELSE 'live' END,
       COALESCE(e.starts_at, e.scheduled_at, e.created_at),
       CASE WHEN e.status = 'ended' THEN COALESCE(e.ends_at, e.updated_at) END,
       e.created_at,
       now()
FROM live_events e
WHERE NOT EXISTS (SELECT 1 FROM live_sessions ls WHERE ls.event_id = e.id);

-- =============================================================================
-- 2. O marcador global.
-- =============================================================================
-- E uma TABELA, e nao uma constante no codigo, por tres motivos: (a) qualquer
-- analista consulta direto no banco; (b) sobrevive a deploy e a rollback de
-- codigo, que e exatamente quando a duvida aparece; (c) permite marcar cortes
-- futuros de outras metricas sem migration nova de schema.
CREATE TABLE IF NOT EXISTS metric_cutovers (
    key          TEXT PRIMARY KEY,
    effective_at TIMESTAMPTZ NOT NULL,
    note         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE metric_cutovers IS
    'D26: registro dos instantes em que uma metrica mudou de definicao ou de fonte. Toda resposta de API que expoe metrica por sessao devolve o effective_at correspondente, para a UI marcar o corte em vez de deixar o leitor concluir que a metrica quebrou.';

INSERT INTO metric_cutovers (key, effective_at, note) VALUES (
    'session_attribution',
    now(),
    'A partir deste instante, unidades e receita por SESSAO saem de cart_item_events (uma linha por adicao, com a sessao que a gerou) e de order_items.session_id (congelado no selamento do pedido). ANTES deste instante saiam de cart_items.session_id, que e FIRST-TOUCH: toda a quantidade de um produto era creditada a sessao da PRIMEIRA adicao. E para eventos anteriores a migration 000090, cart_items.session_id foi backfillado copiando carts.session_id — ou seja, granularidade de EVENTO apresentada como sessao. Comparar os dois lados do corte compara tres definicoes diferentes, nao a mesma metrica em dois periodos.'
) ON CONFLICT (key) DO NOTHING;

-- =============================================================================
-- 3. O marcador POR LINHA: esta sessao tem historico anterior ao corte?
-- =============================================================================
-- O marcador global responde "quando mudou". Este responde "esta transmissao
-- especifica esta contaminada?", que e a pergunta que a tela faz. Uma sessao
-- que nasceu depois do corte tem numeros 100 por cento derivados do log de
-- adicoes e nao precisa de ressalva nenhuma.
ALTER TABLE live_sessions
    ADD COLUMN IF NOT EXISTS attribution_source VARCHAR NOT NULL DEFAULT 'addition_log';

UPDATE live_sessions
SET attribution_source = 'first_touch'
WHERE created_at < (SELECT effective_at FROM metric_cutovers WHERE key = 'session_attribution');

ALTER TABLE live_sessions DROP CONSTRAINT IF EXISTS live_sessions_attribution_source_check;
ALTER TABLE live_sessions ADD CONSTRAINT live_sessions_attribution_source_check
    CHECK (attribution_source IN ('addition_log', 'first_touch'));

COMMENT ON COLUMN live_sessions.attribution_source IS
    'D26: first_touch = a sessao existia antes do corte, entao os numeros dela incluem periodo em que a atribuicao era first-touch e a UI precisa avisar. addition_log = a sessao nasceu depois do corte, numeros derivados do log de adicoes. O DEFAULT e addition_log porque toda sessao criada daqui para frente ja nasce correta.';
