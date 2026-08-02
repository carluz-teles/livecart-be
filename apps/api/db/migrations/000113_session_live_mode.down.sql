-- Reversível: as colunas de origem em live_events permanecem até a 000121, e o
-- que se descarta aqui é estado efêmero.
DROP INDEX IF EXISTS idx_live_sessions_active_product;
ALTER TABLE live_sessions
    DROP COLUMN IF EXISTS current_active_product_id,
    DROP COLUMN IF EXISTS processing_paused;
