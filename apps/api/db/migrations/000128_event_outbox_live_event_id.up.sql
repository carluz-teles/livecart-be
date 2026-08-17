-- Fatia 1/6 do dashboard de telemetria (New Relic): sem live_event_id na
-- própria linha do outbox, nada correlaciona os eventos de um live/campanha
-- sem desserializar o payload de cada produtor — e nem todo produtor carrega
-- o dado no mesmo campo do payload. NULLABLE porque muitos eventos (billing,
-- member, store, ...) não pertencem a um live event.
ALTER TABLE event_outbox
    ADD COLUMN IF NOT EXISTS live_event_id UUID;

COMMENT ON COLUMN event_outbox.live_event_id IS
    'live_events.id do live/campanha que este evento correlaciona (quando aplicável). Distinto de event_id, que identifica a INSTÂNCIA deste evento assíncrono (idempotência) — não confundir.';

-- A consulta canônica do dashboard filtra/agrupa por live event; sem índice
-- ela varreria a tabela inteira a cada janela de tempo.
CREATE INDEX IF NOT EXISTS idx_event_outbox_live_event
    ON event_outbox (live_event_id)
    WHERE live_event_id IS NOT NULL;
