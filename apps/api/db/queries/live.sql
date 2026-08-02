-- =============================================================================
-- LIVE SESSIONS (belong to events, platform-agnostic)
-- =============================================================================

-- name: CreateLiveSession :one
-- sequence_order is MAX+1 per event, computed atomically. The unique index
-- (event_id, sequence_order) catches the rare concurrent-create race.
-- type (D3) é a natureza da transmissão: live|post|reel|story.
INSERT INTO live_sessions (event_id, status, type, sequence_order)
VALUES ($1, $2, $3, COALESCE((SELECT MAX(sequence_order) FROM live_sessions WHERE event_id = $1), 0) + 1)
RETURNING *;

-- name: GetLiveSessionByID :one
SELECT * FROM live_sessions WHERE id = $1;

-- name: GetLiveSessionByIDAndEvent :one
SELECT * FROM live_sessions WHERE id = $1 AND event_id = $2;

-- name: GetActiveSessionByEvent :one
SELECT * FROM live_sessions
WHERE event_id = $1 AND status IN ('active', 'live')
ORDER BY created_at DESC
LIMIT 1;

-- name: StartLiveSession :one
UPDATE live_sessions
SET status = 'live', started_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: EndLiveSession :one
UPDATE live_sessions
SET status = 'ended', ended_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListSessionsByEvent :many
SELECT * FROM live_sessions
WHERE event_id = $1
ORDER BY created_at DESC;

-- name: IncrementLiveSessionComments :exec
UPDATE live_sessions
SET total_comments = total_comments + 1, updated_at = now()
WHERE id = $1;

-- =============================================================================
-- LIVE SESSION PLATFORMS (multiple platform IDs per session)
-- =============================================================================

-- name: AddPlatformToSession :one
INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetSessionByPlatformLiveID :one
-- Find active live session by any associated platform_live_id
SELECT ls.*
FROM live_sessions ls
JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE lsp.platform_live_id = $1 AND ls.status IN ('active', 'live')
ORDER BY ls.created_at DESC
LIMIT 1;

-- name: ListPlatformsBySession :many
SELECT * FROM live_session_platforms
WHERE session_id = $1
ORDER BY added_at;

-- name: RemovePlatformFromSession :exec
DELETE FROM live_session_platforms
WHERE session_id = $1 AND platform_live_id = $2;

-- name: CountPlatformsBySession :one
SELECT COUNT(*)::int FROM live_session_platforms WHERE session_id = $1;

-- name: GetPlatformByLiveID :one
SELECT * FROM live_session_platforms WHERE platform_live_id = $1;

-- name: SetMediaMetadata :exec
-- D1/A4: a legenda/permalink/thumb pertencem à MÍDIA, não ao evento. Chaveado
-- por platform_live_id, que é o media_id do Instagram.
UPDATE live_session_platforms
SET media_permalink = $2, media_thumbnail_url = $3, media_caption = $4
WHERE platform_live_id = $1;

-- name: MarkMediaWebhookActive :exec
-- Desliga o polling DESTA mídia (antes desligava o do evento inteiro, cegando a
-- segunda mídia de um evento guarda-chuva).
UPDATE live_session_platforms
SET webhook_active = true
WHERE platform_live_id = $1;

-- name: ListPollableMedia :many
-- Mídias de post/reel que ainda não receberam webhook de comments e por isso
-- dependem do polling — o único caminho de captura para post sem webhook.
-- Story não entra: resposta de story chega por DM, não por comentário.
SELECT
    lsp.platform_live_id,
    lsp.webhook_active,
    ls.id   AS session_id,
    ls.type AS session_type,
    e.id    AS event_id,
    e.store_id,
    e.status AS event_status
FROM live_session_platforms lsp
JOIN live_sessions ls ON ls.id = lsp.session_id
JOIN live_events e ON e.id = ls.event_id
WHERE ls.type IN ('post', 'reel')
  AND e.status = 'active'
  AND lsp.webhook_active = false
  AND (e.ends_at IS NULL OR e.ends_at >= now() - interval '2 days');


-- =============================================================================
-- MODO LIVE NA SESSÃO (D17) — estado EFÊMERO de execução
--
-- A checagem de posse é loja → evento → sessão: as queries equivalentes no
-- evento casavam (id, store_id) na mesma linha; aqui a loja está a duas tabelas
-- de distância e o JOIN é obrigatório, senão qualquer lojista mexe na sessão de
-- qualquer outro.
-- =============================================================================

-- name: SetSessionActiveProduct :one
UPDATE live_sessions ls
SET current_active_product_id = $2, updated_at = now()
FROM live_events e
WHERE ls.id = $1 AND e.id = ls.event_id AND e.store_id = $3
RETURNING ls.*;

-- name: SetSessionProcessingPaused :one
UPDATE live_sessions ls
SET processing_paused = $2, updated_at = now()
FROM live_events e
WHERE ls.id = $1 AND e.id = ls.event_id AND e.store_id = $3
RETURNING ls.*;

-- name: GetSessionLiveModeState :one
SELECT
    ls.id,
    ls.event_id,
    ls.processing_paused,
    ls.current_active_product_id,
    p.name AS active_product_name,
    p.keyword AS active_product_keyword,
    p.price AS active_product_price,
    p.image_url AS active_product_image_url
FROM live_sessions ls
JOIN live_events e ON e.id = ls.event_id
LEFT JOIN products p ON p.id = ls.current_active_product_id
WHERE ls.id = $1 AND e.store_id = $2;

-- name: SetLiveModeForEventSessions :exec
-- Rota LEGADA por evento: aplica o produto em destaque em todas as sessões
-- vivas do evento. Mantém o painel atual (que só conhece eventId) funcionando
-- enquanto o frontend não passa a mandar sessionId.
UPDATE live_sessions ls
SET current_active_product_id = $2, updated_at = now()
FROM live_events e
WHERE e.id = ls.event_id AND e.id = $1 AND e.store_id = $3
  AND ls.status IN ('active', 'live');

-- name: SetProcessingPausedForEventSessions :exec
-- Idem para a pausa do processamento.
UPDATE live_sessions ls
SET processing_paused = $2, updated_at = now()
FROM live_events e
WHERE e.id = ls.event_id AND e.id = $1 AND e.store_id = $3
  AND ls.status IN ('active', 'live');

-- name: GetEventLiveModeStateFromSessions :one
-- Estado do modo live do EVENTO, lido da sessão viva mais recente. É o que a
-- rota legada devolve enquanto o painel ainda não é por sessão.
SELECT
    ls.id AS session_id,
    ls.processing_paused,
    ls.current_active_product_id,
    p.name AS active_product_name,
    p.keyword AS active_product_keyword,
    p.price AS active_product_price,
    p.image_url AS active_product_image_url
FROM live_sessions ls
JOIN live_events e ON e.id = ls.event_id
LEFT JOIN products p ON p.id = ls.current_active_product_id
WHERE e.id = $1 AND e.store_id = $2
ORDER BY (ls.status IN ('active', 'live')) DESC, ls.sequence_order DESC
LIMIT 1;
