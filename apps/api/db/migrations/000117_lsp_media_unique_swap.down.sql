-- ATENCAO: so funciona ENQUANTO nenhuma midia tiver sido reaproveitada. Depois
-- do primeiro reuso, recriar a UNIQUE global falha. Ponto de nao-retorno MOLE.
--
-- Diagnostico, se falhar:
--   SELECT platform_live_id, count(*) FROM live_session_platforms
--    GROUP BY 1 HAVING count(*) > 1;

ALTER TABLE live_session_platforms
    ADD CONSTRAINT live_session_platforms_platform_live_id_key UNIQUE (platform_live_id);

DROP INDEX IF EXISTS uq_lsp_media_in_flight;
