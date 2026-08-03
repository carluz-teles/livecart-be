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
-- Resolve a sessão dona de uma mídia. SEM filtro de status (D18/D20).
--
-- O filtro ls.status IN ('active','live') que vivia aqui era a única coisa que
-- impedia venda em transmissão encerrada — e, ao mesmo tempo, a causa do
-- descarte SILENCIOSO que é o achado mais caro do ANALISE_LOGS_PRODUCAO: sem
-- linha, ProcessInstagramComment logava um Warn e sumia com o comentário, sem
-- registro para o lojista e sem resposta ao comprador.
--
-- A decisão saiu daqui e virou live.SessionAcceptsPurchase, aplicada depois da
-- resolução. Resolver SEMPRE é o que permite responder.
--
-- ⚠️ A ordenação tem de ser BYTE A BYTE a mesma de GetEventByPlatformLiveID.
-- São duas resoluções independentes pela MESMA chave, e o comentário é gravado
-- com o session_id de uma e o event_id da outra: se elegerem linhas de
-- live_session_platforms diferentes, o comentário fica com a sessão de uma
-- campanha e o evento de outra. Ordenando pelas mesmas colunas de lsp — e
-- desempatando por lsp.id, que é único — as duas elegem a mesma linha por
-- construção. Hoje isso é inócuo (a UNIQUE global garante uma linha só); a
-- ambiguidade nasce com a unique parcial da 000117.
SELECT ls.*
FROM live_sessions ls
JOIN live_session_platforms lsp ON lsp.session_id = ls.id
WHERE lsp.platform_live_id = $1
ORDER BY (lsp.released_at IS NULL) DESC, lsp.added_at DESC, lsp.id DESC
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

-- GetPlatformByLiveID foi REMOVIDA: não tinha nenhum chamador (só o wrapper de
-- repositório, que também não tinha) e era um `:one` sem ORDER BY sobre
-- platform_live_id — a coluna que a 000117 deixou de ter UNIQUE global. Quem a
-- ligasse teria o mesmo bug silencioso das duas queries de resolução: pgx lê a
-- primeira linha e descarta o resto sem erro, então o reuso de um post fixado
-- devolveria uma campanha ARBITRÁRIA. As duas resoluções vivas
-- (GetSessionByPlatformLiveID e GetEventByPlatformLiveID) já ordenam por
-- (released_at IS NULL) DESC, lsp.added_at DESC, lsp.id DESC.

-- name: SetMediaMetadata :exec
-- D1/A4: a legenda/permalink/thumb pertencem à MÍDIA, não ao evento. Chaveado
-- por platform_live_id, que é o media_id do Instagram.
--
-- released_at IS NULL (D22): com a mídia reaproveitável entre campanhas, o
-- platform_live_id deixa de ser único e sem este filtro a legenda da campanha
-- nova sobrescreveria a das campanhas antigas — que é o histórico do lojista.
UPDATE live_session_platforms
SET media_permalink = $2, media_thumbnail_url = $3, media_caption = $4
WHERE platform_live_id = $1 AND released_at IS NULL;

-- name: MarkMediaWebhookActive :exec
-- Desliga o polling DESTA mídia (antes desligava o do evento inteiro, cegando a
-- segunda mídia de um evento guarda-chuva) e só na campanha VIVA — a linha de
-- uma campanha encerrada não é polada e não deve ser tocada.
UPDATE live_session_platforms
SET webhook_active = true
WHERE platform_live_id = $1 AND released_at IS NULL;

-- name: ListPollableMedia :many
-- Mídias que dependem do polling para capturar comentário.
-- Story não entra: resposta de story chega por DM, não por comentário.
--
-- LIVE entra, e entra por uma regra DIFERENTE de post/reel.
--
-- Post/reel: o polling é a ponte ATÉ o webhook assumir, então `webhook_active`
-- desliga o polling daquela mídia — o webhook é confiável ali e a publicação
-- vive para sempre.
--
-- Live: o polling anda JUNTO com o webhook, o tempo todo. A Meta entrega
-- live_comments só "for the duration of the broadcast", e na prática essa
-- entrega FALHA no meio da transmissão: os primeiros comentários viram pedido e
-- os seguintes somem — sem erro, sem log, sem nada, porque o webhook era o
-- ÚNICO caminho de captura da live. Era a maior perda de venda possível, e
-- invisível: ninguém sabe o que não chegou.
--
-- Desligar o polling da live no primeiro webhook (a regra de post) reproduziria
-- exatamente o bug — é o primeiro webhook que chega, é do resto que se perde.
-- Por isso a live ignora `webhook_active`; a duplicata é resolvida pelo dedup
-- por platform_comment_id, que a captura já faz.
--
-- Ler comentário de live pela API só funciona durante a transmissão ("comments
-- on live video broadcasts (outside of the broadcast window) are not
-- returned"), então o polling é limitado à sessão no ar. Encerrar a sessão o
-- desliga — é o segundo motivo para o botão "Encerrar" da transmissão existir.
--
-- O filtro e.status='active' SAIU (D19/D20/A3). Ele era o motivo pelo qual a
-- promessa "nunca fica em silêncio" viraria silêncio TOTAL justamente no
-- evento agendado: sem webhook, esta lista é o único caminho de captura, e um
-- evento que nasce 'scheduled' nunca aparecia nela. Quem decide se vende é o
-- gate de janela (live.WindowAt), depois da captura — a captura precisa ver o
-- comentário para poder respondê-lo.
--
-- A carência para resposta tardia virou 7 dias (N9/RN-37): é o limite do
-- private reply do Instagram, e webhook e polling têm de concordar no número.
-- A carência de 2 dias que estava aqui era inalcançável — o mesmo WHERE exigia
-- status='active' e SweepEndedTimedEvents flipa o evento para 'ended' assim
-- que ends_at vence, então prometia 2 dias e entregava 0.
--
-- live_events não tem ended_at: o carimbo de encerramento é ends_at (que a
-- RN-05 tornou obrigatório e a 000114 backfillou nos legados encerrados), com
-- updated_at como último recurso. Encerramento manual antes do ends_at deixa a
-- janela mais LARGA que 7 dias, nunca mais estreita — erra para o lado de
-- responder.
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
WHERE (
        (ls.type IN ('post', 'reel') AND lsp.webhook_active = false)
     OR (ls.type = 'live' AND ls.status <> 'ended' AND e.status <> 'ended')
  )
  AND (
        e.status <> 'ended'
     OR COALESCE(e.ends_at, e.updated_at) >= now() - interval '7 days'
  );


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

-- =============================================================================
-- MARCADOR DE CORTE DA ATRIBUIÇÃO (D26 / 000121)
-- =============================================================================

-- name: GetMetricCutover :one
-- O instante em que uma métrica mudou de definição. Sem chave conhecida a query
-- devolve pgx.ErrNoRows e o chamador segue sem marcador — a métrica continua
-- respondendo, só sem a ressalva. Falhar aqui derrubaria o relatório inteiro
-- por causa de uma nota de rodapé.
SELECT key, effective_at, note
FROM metric_cutovers
WHERE key = $1;

-- =============================================================================
-- PUBLICAÇÃO AGENDADA (RN-31 / 000123)
-- =============================================================================

-- name: SetSessionPublishAt :exec
-- Registra em live_sessions.publish_at QUANDO esta transmissão foi publicada
-- por agendamento. A coluna existe desde a 000114 e até aqui não tinha
-- escritor nenhum: o agendador é o motivo pelo qual ela foi criada, e sem esta
-- escrita "quando publica" passaria a viver só em session_publish_jobs — duas
-- fontes da verdade para a mesma pergunta, que é o erro que o épico desfaz.
UPDATE live_sessions
SET publish_at = $2, updated_at = now()
WHERE id = $1;

-- name: SetSessionType :exec
-- A campanha nasce sem perguntar a espécie da transmissão: no momento da
-- criação ninguém sabe ainda se aquilo vai ser live, post ou reel. A sessão
-- começa como marcador e aprende o que é quando a publicação é vinculada —
-- o primeiro instante em que a resposta existe.
UPDATE live_sessions SET type = $2 WHERE id = $1;
