-- =============================================================================
-- LIVE EVENTS
-- Container for live sessions. Carts are tied to events, not sessions.
-- =============================================================================

-- CreateLiveEvent SAIU: era o segundo INSERT em live_events, sem nenhum
-- chamador Go, e o unico que nao gravava a janela comercial. Manter dois
-- caminhos de criacao e o que faz a regra nova (ends_at obrigatorio) entrar so
-- em um deles. Quem cria evento e CreateLiveEventFull.

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
--
-- PREFERE O EVENTO VIVO (A5/D22). Com a mídia podendo ser reaproveitada em
-- campanhas diferentes ao longo do tempo (unique parcial da 000115), o mesmo
-- platform_live_id passa a ter N linhas em live_session_platforms e o
-- `:one` do sqlc escolheria uma em SILÊNCIO — pgx lê a primeira e descarta o
-- resto sem erro. released_at IS NULL é exatamente "pertence a um evento vivo",
-- então ordenar por ele é a preferência pedida pelo A5; empatando, a mídia mais
-- recente, e lsp.id (único) fecha o determinismo.
--
-- ⚠️ Esta ordenação é a MESMA de GetSessionByPlatformLiveID, e tem de continuar
-- sendo: ver a nota lá.
SELECT e.*
FROM live_events e
JOIN live_sessions s ON s.event_id = e.id
JOIN live_session_platforms lsp ON lsp.session_id = s.id
WHERE lsp.platform_live_id = $1
ORDER BY (lsp.released_at IS NULL) DESC, lsp.added_at DESC, lsp.id DESC
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
-- LIVE MODE
-- =============================================================================
-- SetActiveProduct, ClearActiveProduct, SetProcessingPaused e GetLiveModeState
-- SAIRAM daqui. O Modo Live desceu para live_sessions na 000111 (D17) e desde
-- entao estas quatro nao tinham chamador nenhum: escreviam e liam colunas de
-- live_events que ninguem mais mantinha, entao ler delas devolvia estado
-- CONGELADO no momento do cutover. Os substitutos vivem em live.sql
-- (SetActiveProductForEventSessions, SetProcessingPausedForEventSessions,
-- GetEventLiveModeStateFromSessions, e o par por sessao).

-- =============================================================================
-- EVENT SCHEDULING & DESCRIPTION
-- =============================================================================

-- name: CreateLiveEventFull :one
-- type SAIU (D3/000120): a especie da campanha e das SESSOES dela, e uma
-- campanha mista nao tem resposta unica no container.
--
-- starts_at/scheduled_at/ends_at ENTRARAM. Antes o evento nascia sem janela e
-- ganhava ends_at num UPDATE posterior, FORA da transacao: se aquele UPDATE
-- falhasse, o evento ja estava commitado sem teto — e evento sem teto e
-- carrinho sem prazo (RN-04 deixa expires_at NULL durante a campanha) e estoque
-- reservado para sempre. Com a coluna NOT NULL desde a 000120, esse buraco
-- deixa de ser possivel: o INSERT falha em vez de o evento nascer torto.
INSERT INTO live_events (
    store_id,
    title,
    status,
    close_cart_on_event_end,
    cart_expiration_minutes,
    cart_max_quantity_per_item,
    send_on_live_end,
    scheduled_at,
    starts_at,
    ends_at,
    description
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
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

-- GetScheduledEvents SAIU: era a terceira query sobre "eventos agendados" e a
-- única sem chamador nenhum, com um predicado diferente das outras duas. Manter
-- três respostas para a mesma pergunta é o que faz o próximo leitor consertar a
-- errada (mesmo motivo da remoção de GetPlatformByLiveID).

-- name: ListEventsReadyToStart :many
-- E37: os eventos AGENDADOS cuja hora marcada já passou e cuja janela ainda não
-- fechou. É o par simétrico de ListEventsPastEndsAt: sem ele nada chama
-- ActivateScheduledEvent, o evento nunca sai de 'scheduled' e o ciclo de vida
-- inteiro de um evento agendado é "não começou" → "encerrado". Ele nunca vende.
--
-- COALESCE(starts_at, scheduled_at): starts_at é a coluna nova (000112) e
-- scheduled_at é a legada que ainda decide o status inicial na criação. As duas
-- são escritas em par por SetEventWindow, mas um evento gravado antes da 000112
-- (ou por UpdateLiveEventDetails, que só toca a legada) pode ter só uma.
--
-- O filtro de ends_at é OBRIGATÓRIO, não higiene: sem ele, um evento cuja janela
-- inteira passou enquanto o serviço estava fora do ar viraria 'active' agora só
-- para o sweep de ends_at o encerrar no ciclo seguinte — duas escritas, um
-- event.ended a mais e, no meio, uma janela em que ele ACEITA compra fora do
-- prazo contratado. Quem passou do fim vai direto para 'ended' pelo outro sweep.
--
-- O LIMIT espelha o de ListEventsPastEndsAt. A versão anterior não tinha nenhum
-- e carregaria a tabela inteira a cada 5 minutos.
SELECT e.id, e.store_id
FROM live_events e
WHERE e.status = 'scheduled'
  AND COALESCE(e.starts_at, e.scheduled_at) IS NOT NULL
  AND COALESCE(e.starts_at, e.scheduled_at) <= now()
  AND (e.ends_at IS NULL OR e.ends_at > now())
ORDER BY COALESCE(e.starts_at, e.scheduled_at) ASC
LIMIT $1;

-- name: ActivateScheduledEvent :execrows
-- Flip 'scheduled' → 'active'. O `AND status = 'scheduled'` é o guard de
-- corrida, não decoração: o botão do lojista e o sweep de 5 em 5 minutos podem
-- cair juntos no mesmo evento (só um escreve), e um evento já encerrado pelo
-- sweep de ends_at nunca volta a ficar ativo.
--
-- Sem store_id de propósito: o sweep é global e não tem loja em mãos. Quem vem
-- pelo caminho do lojista (Service.Start) já validou a posse com GetEventByID
-- antes de chegar aqui.
UPDATE live_events
SET status = 'active', updated_at = now()
WHERE id = $1 AND status = 'scheduled';

-- name: GetLiveEventWithCounts :one
-- product_count conta produtos DISTINTOS nas whitelists das SESSOES (000110):
-- event_products deixa de existir na 000120. DISTINCT e nao COUNT(*) porque o
-- mesmo produto pode estar barrado em varias transmissoes da campanha e o badge
-- responde "quantos produtos", nao "quantas linhas" — a mesma regra de
-- CountEventWhitelistFromSessions, que ja e a fonte da aba Produtos.
SELECT
    e.*,
    (SELECT COUNT(DISTINCT sp.product_id)::int
       FROM session_products sp
       JOIN live_sessions ls ON ls.id = sp.session_id
      WHERE ls.event_id = e.id) AS product_count,
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
-- 'active' virou "não encerrado" (D19/D20): um evento AGENDADO também tem teto,
-- e com o filtro antigo ele ficaria 'scheduled' para sempre com a janela
-- vencida. Continua valendo mesmo com a ativação da E37 no ar — o predicado de
-- ListEventsReadyToStart recusa de propósito quem já passou do fim, então é
-- ESTE sweep que encerra o evento agendado que nunca chegou a abrir. O status
-- ended fica de fora porque encerrar duas vezes é o que este predicado impede.
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
