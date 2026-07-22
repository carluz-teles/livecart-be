-- =============================================================================
-- EVENT OUTBOX
-- =============================================================================
-- Transactional outbox for the async event pipeline. Producers insert in the
-- same tx as the state change; the relay drains pending rows to asynq.

-- name: InsertOutboxEvent :exec
-- Emit-time idempotency: a duplicate dedup_key (webhook retry, at-least-once
-- producer) is a no-op. dedup_key = '' opts out (matches the partial unique
-- index event_outbox_dedup_key_uniq).
INSERT INTO event_outbox (event_id, name, source, metadata, payload, trace_id, span_id, dedup_key, queue, schema_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (dedup_key) WHERE dedup_key <> '' DO NOTHING;

-- name: MarkOutboxDead :exec
-- Dead-letter for the outbox: a row that could not be enqueued after
-- max attempts is parked in 'dead' so ListPendingOutbox stops retrying it and
-- it stays queryable (status='dead', last_error) for inspection/reprocessing.
UPDATE event_outbox
SET status = 'dead', attempts = attempts + 1, last_error = $2
WHERE id = $1;

-- name: ListPendingOutbox :many
-- Oldest-first, row-locked so concurrent relays (or replicas) never grab the
-- same event. SKIP LOCKED keeps the scan non-blocking.
SELECT * FROM event_outbox
WHERE status = 'pending'
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT $1;

-- name: MarkOutboxPublished :exec
UPDATE event_outbox
SET status = 'published', published_at = now()
WHERE id = $1;

-- name: MarkOutboxFailed :exec
UPDATE event_outbox
SET attempts = attempts + 1, last_error = $2
WHERE id = $1;

-- name: TryConsumeEvent :execrows
-- Idempotency guard for consumers without a natural unique key. Returns rows
-- affected: 1 on first consume, 0 if this (event, handler) was already applied.
INSERT INTO event_consumed (event_id, handler)
VALUES ($1, $2)
ON CONFLICT (event_id, handler) DO NOTHING;
