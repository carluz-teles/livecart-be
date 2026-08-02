-- D17 — o estado EFÊMERO de execução (produto em destaque e pausa do
-- processamento) desce do evento para a sessão.
-- Origem: 000034_live_mode_fields.up.sql.
--
-- Motivo: no evento, duas transmissões simultâneas compartilhariam o mesmo
-- produto em destaque, e o estado residual da live de segunda contaminaria a
-- live de quarta do mesmo evento guarda-chuva. Isso é execução, não
-- configuração de campanha.

SET lock_timeout = '5s';

ALTER TABLE live_sessions
    ADD COLUMN IF NOT EXISTS current_active_product_id UUID
        REFERENCES products(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS processing_paused BOOLEAN NOT NULL DEFAULT false;

-- Backfill APENAS de sessão viva: copiar estado efêmero para sessão encerrada
-- ressuscitaria um "produto em destaque" que já não existe.
UPDATE live_sessions ls
SET current_active_product_id = e.current_active_product_id,
    processing_paused         = e.processing_paused
FROM live_events e
WHERE e.id = ls.event_id
  AND ls.status IN ('active', 'live');

CREATE INDEX IF NOT EXISTS idx_live_sessions_active_product
    ON live_sessions (current_active_product_id)
    WHERE current_active_product_id IS NOT NULL;

COMMENT ON COLUMN live_sessions.current_active_product_id IS
    'D17: produto em destaque DESTA transmissão. Fallback quando o comprador comenta com intenção de compra e nenhuma palavra-chave casa.';

COMMENT ON COLUMN live_sessions.processing_paused IS
    'D17: quando true, os comentários DESTA transmissão são gravados mas não viram carrinho. Pausar uma sessão não pausa as outras do mesmo evento.';
