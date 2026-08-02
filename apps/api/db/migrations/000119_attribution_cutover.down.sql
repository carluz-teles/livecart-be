ALTER TABLE live_sessions DROP CONSTRAINT IF EXISTS live_sessions_attribution_source_check;
ALTER TABLE live_sessions DROP COLUMN IF EXISTS attribution_source;
DROP TABLE IF EXISTS metric_cutovers;

-- As sessoes eventualmente criadas pelo reparo 1:1 do passo 1 NAO sao
-- removidas, de proposito: elas sao estruturalmente corretas (todo evento
-- precisa de uma sessao) e apaga-las deixaria carrinho e comentario sem sessao
-- de origem — um estrago maior do que o que o rollback esta desfazendo.
--
-- Se realmente for preciso limpa-las a mao, o filtro e:
--   sequence_order = 1 AND created_at = (o created_at do evento)
