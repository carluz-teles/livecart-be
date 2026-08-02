-- CONTRACT — remove do EVENTO tudo que subiu de nivel. IRREVERSIVEL: o que sai
-- daqui nao existe em nenhum outro lugar em forma recuperavel.
--
-- Nao rodar antes de TODAS estas condicoes serem verdadeiras (secao 6.3 do
-- plano). Estado verificado nesta branch:
--   1. o FE nao le mais event.type — a especie da campanha sai de sessionTypes
--      (livecart-fe/src/lib/event-kind.ts);
--   2. as queries de post/story resolvem por live_session_platforms, nao por
--      live_events.media_id;
--   3. checkout.LoadEventType le live_sessions.type — e a tela do COMPRADOR,
--      que quebraria antes do painel;
--   4. a whitelist le session_products (000110);
--   5. o Modo Live e da sessao (000111); as quatro queries de evento estavam
--      sem chamador desde entao;
--   6. ends_at e obrigatorio na aplicacao desde a RN-05 e agora entra no
--      proprio INSERT, dentro da transacao de criacao;
--   7. backup verificado e restauravel no mesmo dia — responsabilidade do
--      deploy, nao desta migration;
--   8. o trigger da 000114 mantem live_session_platforms.released_at, entao
--      nenhuma midia fica presa.
--
-- ORDEM: a 000119 LE live_events.type no reparo 1:1. Ela tem de ter rodado.

SET lock_timeout = '5s';

-- =============================================================================
-- 1. ends_at obrigatorio (D5/RN-05)
-- =============================================================================
-- Backfill final dos legados que sobraram vivos: fecha no maior fim de sessao
-- conhecido, ou em updated_at. Nenhum dos dois e a data "certa" — a data certa
-- nao existe para um evento que nunca teve teto. O que importa e que ele passe
-- a TER um, porque enquanto ends_at e NULL a RN-04 mantem expires_at NULL e o
-- carrinho fica sem prazo para sempre, com o estoque reservado junto.
--
-- Por que o NOT NULL so entra AGORA, e nao como CHECK NOT VALID la atras: em
-- Postgres, uma constraint CHECK NOT VALID e aplicada em qualquer UPDATE de
-- linha existente, nao so em INSERT. Um CHECK (ends_at IS NOT NULL) NOT VALID
-- teria feito TODO update de evento legado falhar — inclusive
-- IncrementLiveEventOrders, que roda no caminho de PAGAMENTO. Isso derrubaria
-- vendas para impor uma regra de cadastro.
UPDATE live_events e
SET ends_at = COALESCE(
        (SELECT MAX(ls.ended_at) FROM live_sessions ls WHERE ls.event_id = e.id),
        e.updated_at)
WHERE e.ends_at IS NULL;

ALTER TABLE live_events ALTER COLUMN ends_at SET NOT NULL;

COMMENT ON COLUMN live_events.ends_at IS
    'D5/RN-05: TETO da campanha, obrigatorio. E ele que garante que nenhum carrinho fica sem prazo — durante o evento expires_at fica NULL por definicao (RN-04) e o relogio so comeca quando a janela fecha.';

-- =============================================================================
-- 2. type e metadados de midia saem do evento (D3 + D1)
-- =============================================================================
-- A verdade esta em live_sessions.type e em live_session_platforms.media_*
-- desde a 000109. Manter a copia no evento nao era redundancia inofensiva: um
-- evento guarda-chuva tem N midias e N tipos, entao a coluna do container
-- respondia por UMA delas — a primeira — e apresentava isso como se fosse a
-- resposta da campanha.
DROP INDEX IF EXISTS idx_live_events_type;
DROP INDEX IF EXISTS idx_live_events_media_id;
ALTER TABLE live_events
    DROP COLUMN IF EXISTS type,
    DROP COLUMN IF EXISTS media_id,
    DROP COLUMN IF EXISTS media_permalink,
    DROP COLUMN IF EXISTS media_thumbnail_url,
    DROP COLUMN IF EXISTS media_caption,
    DROP COLUMN IF EXISTS webhook_active;

-- =============================================================================
-- 3. Modo live sai do evento (D17)
-- =============================================================================
-- Estado EFEMERO de execucao, nao configuracao de campanha. Vive em
-- live_sessions desde a 000111. Duas transmissoes simultaneas compartilhavam o
-- mesmo produto em destaque, e o estado residual de segunda contaminava a live
-- de quarta.
DROP INDEX IF EXISTS idx_live_events_active_product;
ALTER TABLE live_events
    DROP COLUMN IF EXISTS current_active_product_id,
    DROP COLUMN IF EXISTS processing_paused;

-- =============================================================================
-- 4. Whitelist sai do evento (D15)
-- =============================================================================
-- A verdade esta em session_products desde a 000110, e a sessao nova herda a
-- lista do evento no nascimento. Deixar event_products de pe seria manter uma
-- segunda whitelist que ninguem escreve e que "lista vazia libera tudo"
-- transforma em barreira invisivel.
DROP TABLE IF EXISTS event_products;

-- =============================================================================
-- 5. Os COMENTARIOS que esta migration acabou de tornar falsos
-- =============================================================================
-- As 000109/000110/000112 documentaram as colunas dizendo "ate a 000119" — a
-- numeracao do plano, que deslocou em um quando a 000116 foi usada pelo motivo
-- de nao entrega. Pior que o numero errado e a frase: elas prometem que
-- live_events.type ainda existe. Um comentario de coluna e a primeira coisa que
-- alguem le no \d, e um que mente sobre o schema atual custa mais caro que
-- comentario nenhum.
COMMENT ON COLUMN live_sessions.type IS
    'D3: natureza da transmissao (live|post|reel|story). UNICA fonte da verdade do tipo — live_events.type foi dropada na 000120. O vocabulario antigo (single|multi) so existe na entrada da API e e traduzido em live.SessionTypeFromEventType.';

COMMENT ON COLUMN live_events.starts_at IS
    'D21: inicio da JANELA COMERCIAL da campanha. Nao e a data de publicacao da midia (essa e live_sessions.publish_at). Escrita SEMPRE em par com scheduled_at, que continua sendo a coluna que EffectiveStatus le.';
