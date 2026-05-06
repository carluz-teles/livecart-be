-- Customer-facing order timeline. Append-only event store separate from
-- shipment_tracking_events (which is provider-flavored carrier data).
-- order_events is the canonical timeline the public tracking page renders
-- and the postcheckout email flow consumes.
--
-- We pin a small enum on event_type to avoid drift; new event types arrive
-- with their own migration so consumers don't silently miss them.

CREATE TABLE IF NOT EXISTS order_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id       UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    event_type    TEXT NOT NULL CHECK (event_type IN (
        'payment_confirmed',
        'shipped',
        'delivered'
    )),
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Where the event came from. 'system' = BE auto-emitted, 'merchant' =
    -- lojista clicou um botão no dashboard, 'customer' = cliente confirmou
    -- na página pública. Useful for audit and for diverging notification
    -- behavior (we don't want to send "obrigado" emails for a self-confirm).
    source        TEXT NOT NULL DEFAULT 'system' CHECK (source IN ('system', 'merchant', 'customer')),
    -- Structured payload (tracking_code, carrier name, marker who clicked, …).
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Listing the timeline for a cart in chronological order is the dominant
-- read pattern from the public order page.
CREATE INDEX IF NOT EXISTS idx_order_events_cart_time
    ON order_events (cart_id, occurred_at);

-- One event of each type per cart. Re-emitting 'shipped' would multiply
-- emails on a webhook retry; the upsert pattern in Go uses ON CONFLICT DO
-- NOTHING against this constraint to stay idempotent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_order_events_cart_type_unique
    ON order_events (cart_id, event_type);
