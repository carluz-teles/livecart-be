-- One mutable checkpoint per integration, not an append-only execution log.
-- The catalog snapshot is released when the run finishes.
CREATE TABLE erp_resync_jobs (
    integration_id UUID PRIMARY KEY REFERENCES integrations(id) ON DELETE CASCADE,
    run_id UUID NOT NULL,
    dispatch_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','running','retrying','completed','completed_with_errors','failed')),
    product_ids TEXT[] NOT NULL DEFAULT '{}',
    total INTEGER NOT NULL CHECK (total >= 0),
    processed INTEGER NOT NULL DEFAULT 0 CHECK (processed >= 0 AND processed <= total),
    succeeded INTEGER NOT NULL DEFAULT 0 CHECK (succeeded >= 0),
    failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0),
    consecutive_retries INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    lease_owner UUID,
    lease_until TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    CHECK (succeeded + failed = processed)
);
