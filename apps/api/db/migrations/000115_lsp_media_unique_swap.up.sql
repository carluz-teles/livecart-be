-- D22, fase 2 de 2 — a midia deixa de ser unica GLOBALMENTE e passa a ser unica
-- ENTRE AS EM USO. E o que permite reaproveitar um post fixado em campanhas
-- diferentes ao longo do tempo, sem nunca ter duas campanhas VIVAS disputando a
-- mesma midia (que e a ambiguidade de roteamento que a D22 proibe).
--
-- PRE-REQUISITO DE DEPLOY, nao opcional: o trigger da 000114 tem de estar de pe
-- e released_at coerente. Sem ele, midia de evento encerrado DEPOIS desta
-- migration nunca libera — e o sintoma e "o reuso nao funciona", sem erro
-- nenhum. Conferir antes (esperado: 0):
--
--   SELECT count(*) FROM live_session_platforms lsp
--     JOIN live_sessions ls ON ls.id = lsp.session_id
--     JOIN live_events e ON e.id = ls.event_id
--    WHERE e.status = 'ended' AND lsp.released_at IS NULL;
--
-- E o codigo de resolucao ja tem de preferir a campanha viva: as duas queries
-- ordenam por (released_at IS NULL) DESC e desempatam pela mesma coluna, senao
-- o `:one` do sqlc escolhe uma linha em silencio.
--
-- Seguranca: igual a 000105, o indice novo cobre um SUBCONJUNTO das linhas da
-- constraint atual. Nao pode falhar por dado pre-existente.
--
-- Nome fisico da constraint atual: live_session_platforms_platform_live_id_key,
-- gerado pelo UNIQUE(platform_live_id) inline em 000018.
--
-- Ordem proposital: cria o indice novo ANTES de dropar a constraint.

SET lock_timeout = '5s';

CREATE UNIQUE INDEX IF NOT EXISTS uq_lsp_media_in_flight
    ON live_session_platforms (platform_live_id)
    WHERE released_at IS NULL;

ALTER TABLE live_session_platforms
    DROP CONSTRAINT IF EXISTS live_session_platforms_platform_live_id_key;

-- idx_live_session_platforms_platform_live_id (000018) NAO cai: a resolucao
-- passou a ler midia LIBERADA tambem (D20 tirou o filtro de status), e o indice
-- parcial acima nao serve essas linhas.

COMMENT ON INDEX uq_lsp_media_in_flight IS
    'D22: uma midia pertence a no maximo UM evento vivo por vez. Midias liberadas (released_at NOT NULL) podem ser reaproveitadas numa campanha nova. O predicado so e expressavel porque a 000114 denormalizou live_events.status em released_at.';
