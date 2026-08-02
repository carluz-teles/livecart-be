-- Reversivel integralmente: a 000114 nao muda comportamento de resolucao nenhum,
-- so passa a manter released_at coerente.
DROP TRIGGER IF EXISTS trg_set_lsp_released_at_on_insert ON live_session_platforms;
DROP FUNCTION IF EXISTS set_lsp_released_at_on_insert();
DROP TRIGGER IF EXISTS trg_sync_lsp_released_at ON live_events;
DROP FUNCTION IF EXISTS sync_lsp_released_at();
ALTER TABLE live_session_platforms DROP COLUMN IF EXISTS released_at;
