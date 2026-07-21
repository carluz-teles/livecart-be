-- Transactional outbox for the async event pipeline. Domain transitions write
-- the event in the SAME transaction as the state change; a relay then enqueues
-- pending rows to asynq (Redis) and marks them published. This is what makes
-- delivery survive a crash between the state commit and the enqueue.
CREATE TABLE IF NOT EXISTS event_outbox (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Unique per event instance: dedups a relay that re-enqueues after a crash
    -- and is reused as the asynq task id.
    event_id     UUID NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    trace_id     TEXT NOT NULL DEFAULT '',
    span_id      TEXT NOT NULL DEFAULT '',
    dedup_key    TEXT NOT NULL DEFAULT '',
    queue        TEXT NOT NULL DEFAULT 'normal',
    status       TEXT NOT NULL DEFAULT 'pending', -- pending | published | failed
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);

-- The relay only scans unpublished rows, oldest first. A partial index keeps
-- that hot-path read cheap as the table grows.
CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (created_at)
    WHERE status = 'pending';

-- Generic idempotency guard for consumers that lack a natural unique key on
-- their side-effect. A handler records (event_id, handler) before applying, so
-- an at-least-once redelivery is a no-op.
CREATE TABLE IF NOT EXISTS event_consumed (
    event_id    UUID NOT NULL,
    handler     TEXT NOT NULL,
    consumed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, handler)
);
