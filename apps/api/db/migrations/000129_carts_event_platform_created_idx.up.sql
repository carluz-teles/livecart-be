-- Fatia 4/6 do dashboard de telemetria (New Relic): correlaciona comentários
-- (live_comments) com carrinhos criados subsequentemente, via
-- event_id + platform_user_id + created_at dentro de uma janela de 120s
-- (comment.received listener em internal/telemetry/exporter).
--
-- A correlação é por event_id (a CAMPANHA), não só session_id (a transmissão):
-- o comprador pode comentar numa live e só efetivar o carrinho numa live
-- seguinte da MESMA campanha, e o join precisa achar esse carrinho também.
-- Sem este índice, a query faria seq scan em carts a cada comentário.
CREATE INDEX IF NOT EXISTS idx_carts_event_platform_created
    ON carts (event_id, platform_user_id, created_at);
