-- =============================================================================
-- LIVE EVENTS
-- Container for live sessions. Carts are tied to events, not sessions.
-- =============================================================================

-- name: CreateLiveEvent :one
INSERT INTO live_events (
    store_id,
    title,
    type,
    status,
    close_cart_on_event_end,
    cart_expiration_minutes,
    cart_max_quantity_per_item,
    send_on_live_end
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetLiveEventByID :one
SELECT * FROM live_events WHERE id = $1;

-- name: GetLiveEventByIDAndStore :one
SELECT * FROM live_events WHERE id = $1 AND store_id = $2;

-- name: GetActiveLiveEventByStore :one
SELECT * FROM live_events
WHERE store_id = $1 AND status = 'active'
ORDER BY created_at DESC
LIMIT 1;

-- name: EndLiveEvent :one
UPDATE live_events
SET status = 'ended', updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: UpdateLiveEventTitle :one
UPDATE live_events
SET title = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListLiveEventsByStore :many
SELECT * FROM live_events
WHERE store_id = $1
ORDER BY created_at DESC;

-- name: IncrementLiveEventOrders :exec
UPDATE live_events
SET total_orders = total_orders + 1, updated_at = now()
WHERE id = $1;

-- name: CountSessionsByEvent :one
SELECT COUNT(*)::int FROM live_sessions WHERE event_id = $1;

-- name: GetEventBySessionID :one
SELECT e.* FROM live_events e
JOIN live_sessions s ON s.event_id = e.id
WHERE s.id = $1;

-- name: GetEventByPlatformLiveID :one
-- Resolve o evento dono de uma mídia (via sessão). SEM filtro de status
-- (D19/D20).
--
-- O e.status='active' que vivia aqui era o que fazia o comentário em campanha
-- agendada ou encerrada virar um Warn e sumir: sem evento não há store_id, não
-- há como responder e não há como registrar. Três casos, um padrão só —
-- resolve sempre, decide depois (live.WindowAt), nunca fica em silêncio.
SELECT e.*
FROM live_events e
JOIN live_sessions s ON s.event_id = e.id
JOIN live_session_platforms lsp ON lsp.session_id = s.id
WHERE lsp.platform_live_id = $1
ORDER BY e.created_at DESC
LIMIT 1;

-- name: GetEventCartSettings :one
-- FONTE ÚNICA do prazo do carrinho: override do evento com fallback para o
-- default da loja. Existia desde sempre e nunca teve chamador — enquanto isso o
-- mesmo COALESCE estava inline no FinalizeCartsByEvent e uma versão degradada,
-- só-loja, em order/repository.go (GetStoreCartExpirationMinutes). Agora os dois
-- consomem daqui.
--
-- RN-34 — close_cart_on_event_end deixa de ser "ter x não ter prazo" e passa a
-- escolher QUAL dos dois prazos vale:
--   ligado    → prazo curto (cart_expiration_minutes)
--   desligado → prazo estendido (cart_extended_expiration_minutes, 000104)
-- Os DOIS ramos armam cart.expire pelo mesmo mecanismo. Nada fica eterno e
-- nenhum sweep de carrinhos precisa voltar. O ramo antigo "0 = preserva o
-- expires_at que havia" saiu: a 000104 pôs CHECK >= 15 nas duas pontas, então
-- o valor efetivo nunca é 0 — e sob a RN-04 o expires_at preservado seria NULL
-- por definição, ou seja, carrinho eterno exatamente no fechamento.
SELECT
    e.id AS event_id,
    e.store_id,
    e.close_cart_on_event_end,
    COALESCE(e.cart_expiration_minutes, s.cart_expiration_minutes) AS cart_expiration_minutes,
    COALESCE(e.cart_extended_expiration_minutes, s.cart_extended_expiration_minutes) AS cart_extended_expiration_minutes,
    (CASE WHEN e.close_cart_on_event_end
          THEN COALESCE(e.cart_expiration_minutes, s.cart_expiration_minutes)
          ELSE COALESCE(e.cart_extended_expiration_minutes, s.cart_extended_expiration_minutes)
     END)::int AS effective_cart_expiration_minutes,
    COALESCE(e.cart_max_quantity_per_item, s.cart_max_quantity_per_item) AS cart_max_quantity_per_item,
    COALESCE(e.send_on_live_end, s.send_on_live_end) AS send_on_live_end,
    e.waitlist_notified_ttl_minutes
FROM live_events e
JOIN stores s ON s.id = e.store_id
WHERE e.id = $1;

-- name: GetWaitlistNotifiedTTLByEvent :one
SELECT waitlist_notified_ttl_minutes FROM live_events WHERE id = $1;

-- =============================================================================
-- LIVE MODE - Active Product and Processing Control
-- =============================================================================

-- name: SetActiveProduct :one
UPDATE live_events
SET current_active_product_id = $2, updated_at = now()
WHERE id = $1 AND store_id = $3
RETURNING *;

-- name: ClearActiveProduct :one
UPDATE live_events
SET current_active_product_id = NULL, updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: SetProcessingPaused :one
UPDATE live_events
SET processing_paused = $2, updated_at = now()
WHERE id = $1 AND store_id = $3
RETURNING *;

-- name: GetLiveModeState :one
SELECT
    e.id,
    e.processing_paused,
    e.current_active_product_id,
    p.name AS active_product_name,
    p.keyword AS active_product_keyword,
    p.price AS active_product_price,
    p.image_url AS active_product_image_url
FROM live_events e
LEFT JOIN products p ON p.id = e.current_active_product_id
WHERE e.id = $1 AND e.store_id = $2;

-- =============================================================================
-- EVENT SCHEDULING & DESCRIPTION
-- =============================================================================

-- name: CreateLiveEventFull :one
INSERT INTO live_events (
    store_id,
    title,
    type,
    status,
    close_cart_on_event_end,
    cart_expiration_minutes,
    cart_max_quantity_per_item,
    send_on_live_end,
    scheduled_at,
    description
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: UpdateLiveEventDetails :one
UPDATE live_events
SET
    title = COALESCE($3, title),
    description = $4,
    scheduled_at = $5,
    updated_at = now()
WHERE id = $1 AND store_id = $2
RETURNING *;

-- name: GetScheduledEvents :many
SELECT * FROM live_events
WHERE store_id = $1 AND scheduled_at IS NOT NULL AND status = 'scheduled'
ORDER BY scheduled_at ASC;

-- name: ListEventsReadyToStart :many
-- Find scheduled events that should be started (scheduled_at <= now)
SELECT * FROM live_events
WHERE status = 'scheduled' AND scheduled_at <= now()
ORDER BY scheduled_at ASC;

-- name: ActivateScheduledEvent :one
UPDATE live_events
SET status = 'active', updated_at = now()
WHERE id = $1 AND status = 'scheduled'
RETURNING *;

-- name: GetLiveEventWithCounts :one
SELECT
    e.*,
    (SELECT COUNT(*)::int FROM event_products WHERE event_id = e.id) AS product_count,
    (SELECT COUNT(*)::int FROM event_upsells WHERE event_id = e.id) AS upsell_count
FROM live_events e
WHERE e.id = $1 AND e.store_id = $2;

-- name: ListEventsPastEndsAt :many
-- D5/RN-05: TODO evento cuja janela (ends_at) já fechou mas que continua
-- 'active' porque nada dispara o encerramento (EffectiveStatus é só derivado em
-- leitura). O sweep chama live.Service.End em cada um para finalizar carts
-- (armar expires_at) e o ERP.
--
-- O filtro por tipo SAIU. O comentário anterior justificava restringir a
-- post/reel/story para "não auto-encerrar lives agendadas que o lojista quer
-- manter rodando além do horário nominal" — essa decisão de produto foi
-- REVOGADA pela RN-05: ends_at deixou de ser horário nominal e virou o TETO
-- contratual da campanha. É ele que garante que nenhum carrinho fica órfão,
-- porque a RN-04 mantém expires_at NULL enquanto o evento está aberto. Com o
-- filtro no lugar, um evento de live com ends_at vencido nunca fechava e seus
-- carrinhos nunca ganhavam prazo.
--
-- Esta é a REDE. O caminho preciso é a task ETA event.window_close, armada na
-- criação e re-armada na edição de ends_at.
--
-- 'active' virou "não encerrado" (D19/D20): um evento AGENDADO que nunca foi
-- ativado — e nada o ativa sozinho, ActivateScheduledEvent não tem chamador —
-- também tem teto, e com o filtro antigo ele ficaria 'scheduled' para sempre
-- com a janela vencida. O status ended continua de fora porque encerrar duas
-- vezes é o que este predicado impede.
SELECT e.id, e.store_id
FROM live_events e
WHERE e.status <> 'ended'
  AND e.ends_at IS NOT NULL
  AND e.ends_at < now()
ORDER BY e.ends_at ASC
LIMIT $1;

-- name: GetActiveTimedEventByMediaID :one
-- Resolve o evento AINDA NÃO ENCERRADO dono de uma mídia de post/reel/story
-- (mídia apagada no Instagram) para roteá-lo por End (finaliza carts + ERP),
-- não só flipar status.
-- Resolve pela MÍDIA (live_session_platforms), não mais por live_events.media_id.
--
-- 'active' virou "não encerrado" pelo mesmo motivo do ListPollableMedia: agora
-- que o polling enxerga evento agendado, a mídia apagada de um evento agendado
-- também precisa parar o loop — senão ele martela um media id morto a cada 20s
-- até a data de início chegar.
SELECT e.id, e.store_id
FROM live_events e
JOIN live_sessions ls ON ls.event_id = e.id
JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE lsp.platform_live_id = $1
  AND e.status <> 'ended'
  AND ls.type IN ('post', 'reel', 'story')
LIMIT 1;
