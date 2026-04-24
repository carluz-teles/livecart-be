-- Shipments persist the provider-created freight order so the admin UI can
-- resume the logistics flow (attach invoice, generate labels, refresh tracking)
-- after the HTTP handlers finish. The `provider_meta` JSONB snapshots the raw
-- response for debugging — never read by business logic.
--
-- shipment_tracking_events is append-only; each pull of /tracking (or, in the
-- future, each webhook) produces one or more events. The caller is responsible
-- for deduping against (shipment_id, event_at, raw_code) before inserting.

CREATE TABLE IF NOT EXISTS shipments (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id              UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    store_id              UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,

    provider              VARCHAR NOT NULL,               -- 'melhor_envio' | 'smartenvios' | ...
    provider_order_id     VARCHAR NOT NULL,               -- freight_order_id at the provider
    provider_order_number VARCHAR,                        -- human-readable number for admin UI
    tracking_code         VARCHAR,
    public_tracking_url   VARCHAR,

    invoice_key           VARCHAR,                        -- NFe/DCe chave when linked
    invoice_kind          VARCHAR,                        -- 'nfe' | 'dce'
    invoice_id            VARCHAR,                        -- provider-side invoice id, if any

    label_url             VARCHAR,

    status                VARCHAR NOT NULL DEFAULT 'pending', -- normalized LiveCart TrackingStatus
    status_raw_code       INTEGER,
    status_raw_name       VARCHAR,

    provider_meta         JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (provider, provider_order_id)
);

CREATE INDEX IF NOT EXISTS shipments_order_id_idx ON shipments (order_id);
CREATE INDEX IF NOT EXISTS shipments_store_id_idx ON shipments (store_id);
CREATE INDEX IF NOT EXISTS shipments_tracking_code_idx ON shipments (tracking_code);

CREATE TABLE IF NOT EXISTS shipment_tracking_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shipment_id  UUID NOT NULL REFERENCES shipments(id) ON DELETE CASCADE,

    status       VARCHAR NOT NULL,       -- normalized LiveCart TrackingStatus
    raw_code     INTEGER,
    raw_name     VARCHAR,
    observation  TEXT,

    event_at     TIMESTAMPTZ NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    source       VARCHAR NOT NULL DEFAULT 'poll',   -- 'poll' | 'webhook'

    UNIQUE (shipment_id, event_at, raw_code)
);

CREATE INDEX IF NOT EXISTS shipment_tracking_events_shipment_idx
    ON shipment_tracking_events (shipment_id, event_at DESC);

COMMENT ON TABLE  shipments IS 'Provider-agnostic freight orders linked to carts. One row per shipment created at a carrier.';
COMMENT ON TABLE  shipment_tracking_events IS 'Append-only timeline of tracking events per shipment. Source = poll (pull) | webhook (future).';
