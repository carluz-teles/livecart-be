-- Restore the legacy schema for an application rollback. Deleted log entries
-- cannot be recovered by this migration.
CREATE TABLE public.integration_logs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id   UUID NOT NULL REFERENCES public.integrations(id) ON DELETE CASCADE,
    entity_type      VARCHAR,
    entity_id        UUID,
    direction        VARCHAR,
    status           VARCHAR,
    request_payload  JSONB,
    response_payload JSONB,
    error_message    TEXT,
    created_at       TIMESTAMPTZ DEFAULT now()
);
