-- =============================================================================
-- SESSION PUBLISH JOBS (RN-31 / N3)
-- Agendamento de publicacao no Instagram. O agendador e nosso: a Graph nao tem
-- scheduled_publish_time e o container expira em 24h.
-- =============================================================================

-- name: CreateSessionPublishJob :one
INSERT INTO session_publish_jobs (
    store_id, media_kind, asset_path, asset_content_type,
    caption, title, product_ids,
    starts_at, ends_at, cart_expiration_minutes, cart_max_quantity_per_item,
    scheduled_for
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetSessionPublishJob :one
SELECT * FROM session_publish_jobs WHERE id = $1 AND store_id = $2;

-- name: ListSessionPublishJobsByStore :many
SELECT * FROM session_publish_jobs
WHERE store_id = $1
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
ORDER BY scheduled_for DESC
LIMIT $2;

-- ClaimSessionPublishJob e a REIVINDICACAO atomica do disparo. O guard
-- `status = 'scheduled'` e o que faz o cancelamento valer mesmo quando a task
-- asynq ja foi entregue: quem cancelou moveu a linha, e este UPDATE nao acha
-- nada. Tambem e o que impede publicacao dupla se a task for entregue duas
-- vezes (asynq e at-least-once).
-- name: ClaimSessionPublishJob :one
UPDATE session_publish_jobs
SET status = 'publishing',
    attempts = attempts + 1,
    last_attempt_at = now(),
    updated_at = now()
WHERE id = $1 AND status = 'scheduled'
RETURNING *;

-- MarkSessionPublishJobPublished aceita last_error preenchido de proposito. O
-- desfecho "publicou mas nao conseguiu criar o evento" e REAL: o post ja esta
-- no ar e retentar republicaria. Ele precisa terminar como 'published' (para
-- nao ser reivindicado de novo) carregando o motivo de o vinculo ter faltado.
-- name: MarkSessionPublishJobPublished :exec
UPDATE session_publish_jobs
SET status = 'published',
    published_media_id = $2,
    event_id = sqlc.narg(event_id),
    session_id = sqlc.narg(session_id),
    published_at = now(),
    last_error = sqlc.narg(last_error),
    updated_at = now()
WHERE id = $1;

-- ReleaseSessionPublishJob devolve o job para a fila depois de uma tentativa
-- que falhou mas ainda tem retry. Volta para 'scheduled' de proposito: e o
-- unico estado que ClaimSessionPublishJob reivindica, entao o retry do asynq E
-- o sweep de backstop enxergam a mesma linha.
-- name: ReleaseSessionPublishJob :exec
UPDATE session_publish_jobs
SET status = 'scheduled',
    last_error = $2,
    updated_at = now()
WHERE id = $1 AND status = 'publishing';

-- name: FailSessionPublishJob :exec
UPDATE session_publish_jobs
SET status = 'failed',
    last_error = $2,
    updated_at = now()
WHERE id = $1 AND status IN ('scheduled', 'publishing');

-- CancelSessionPublishJob so alcanca o que ainda NAO foi reivindicado. Um job
-- em 'publishing' ja pode ter criado o container na Graph — cancelar ali
-- deixaria o post no ar com o agendamento marcado como cancelado.
-- name: CancelSessionPublishJob :one
UPDATE session_publish_jobs
SET status = 'cancelled',
    cancelled_at = now(),
    updated_at = now()
WHERE id = $1 AND store_id = $2 AND status = 'scheduled'
RETURNING *;

-- ListDueSessionPublishJobs e o BACKSTOP do agendador ETA: task perdida (Redis
-- limpo, deploy no meio) deixa o job vencido e ninguem o dispara. Mesmo papel
-- do SweepEndedTimedEvents.
-- name: ListDueSessionPublishJobs :many
SELECT * FROM session_publish_jobs
WHERE status = 'scheduled' AND scheduled_for <= now()
ORDER BY scheduled_for ASC
LIMIT $1;

-- ListStuckSessionPublishJobs acha o job preso em 'publishing': o processo
-- morreu entre a reivindicacao e o desfecho, entao nenhum estado terminal foi
-- gravado e a linha nunca mais e reivindicada. Sem isto o agendamento morre em
-- silencio, que e o pior desfecho possivel para uma publicacao.
-- name: ListStuckSessionPublishJobs :many
SELECT * FROM session_publish_jobs
WHERE status = 'publishing' AND last_attempt_at < $1
ORDER BY last_attempt_at ASC
LIMIT $2;
