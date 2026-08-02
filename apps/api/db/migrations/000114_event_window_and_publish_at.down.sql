DROP INDEX IF EXISTS idx_live_sessions_publish_at;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS publish_at;

DROP INDEX IF EXISTS idx_live_events_starts_at;
ALTER TABLE live_events DROP COLUMN IF EXISTS starts_at;

COMMENT ON COLUMN live_events.ends_at IS NULL;

-- ATENCAO: o ends_at gravado nos eventos ENCERRADOS nao e revertido de
-- proposito. E dado derivado e correto (a data em que o evento de fato acabou),
-- e apaga-lo destruiria informacao que nao estava la antes mas que tambem nao
-- faz mal nenhum. Se for mesmo necessario desfazer, o filtro e:
--   status = 'ended' AND ends_at = updated_at
