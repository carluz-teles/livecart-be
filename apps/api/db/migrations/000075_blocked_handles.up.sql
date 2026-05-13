-- blocked_handles tracks Instagram handles a store has blocked from buying.
-- A row exists per (store, handle) and is "active" while unblocked_at IS NULL.
-- Unblocking does not delete the row so we keep audit trail and can reactivate
-- by setting unblocked_at back to NULL.
CREATE TABLE IF NOT EXISTS blocked_handles (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id            UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    platform_handle     VARCHAR NOT NULL,
    reason              TEXT,
    blocked_by_user_id  UUID REFERENCES users(id),
    blocked_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    unblocked_at        TIMESTAMPTZ,
    unblocked_by_user_id UUID REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (store_id, platform_handle)
);

CREATE INDEX IF NOT EXISTS idx_blocked_handles_active
    ON blocked_handles (store_id, platform_handle)
    WHERE unblocked_at IS NULL;

-- cancelled_reason explains WHY a cart left the open flow without being paid.
-- Used by the block flow to mark carts as 'customer_blocked' so the public
-- /carts/by-token/:token endpoint can hide them, and the order detail page
-- can show a "cliente bloqueado" badge.
ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS cancelled_reason VARCHAR;
