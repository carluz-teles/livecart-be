-- Coupons are scoped to a single live_event — a merchant runs one campaign
-- per live ("LIVE10") and codes from a previous event don't bleed into the
-- next. The event_id FK with ON DELETE CASCADE is the source of truth: when
-- the merchant deletes a draft event, all its coupons go with it. Carts
-- referencing a coupon (via coupon_redemptions) are protected because the
-- redemption row's coupon_id sets ON DELETE RESTRICT — we cannot lose the
-- audit trail of an applied coupon while the cart still references it.

CREATE TABLE IF NOT EXISTS coupons (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id            UUID NOT NULL REFERENCES live_events(id) ON DELETE CASCADE,
    -- Code as the customer types it (case-insensitive uniqueness enforced
    -- below). We keep the original casing for display ("LiveSummer10") and
    -- match via lower() at apply time.
    code                TEXT NOT NULL,
    -- 'percent' applies percent_bps (basis points: 1000 = 10%) on the cart
    -- subtotal. 'fixed' subtracts value_cents. 'free_shipping' zeroes the
    -- shipping line on the cart at apply time.
    type                TEXT NOT NULL CHECK (type IN ('percent', 'fixed', 'free_shipping')),
    -- Either value_cents (fixed) or percent_bps (percent) is set; both stay 0
    -- for free_shipping. Service layer enforces the right shape per type.
    value_cents         BIGINT NOT NULL DEFAULT 0,
    percent_bps         INTEGER NOT NULL DEFAULT 0,
    -- Caps below are nullable so "unlimited" is a real state.
    max_uses            INTEGER,
    used_count          INTEGER NOT NULL DEFAULT 0,
    min_purchase_cents  BIGINT NOT NULL DEFAULT 0,
    valid_from          TIMESTAMPTZ,
    valid_until         TIMESTAMPTZ,
    active              BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Code uniqueness scoped to event + case-insensitive. Two events can both
-- define "LIVE10"; within the same event "LIVE10" and "live10" collide.
CREATE UNIQUE INDEX IF NOT EXISTS coupons_event_code_lower_idx
    ON coupons (event_id, lower(code));

CREATE INDEX IF NOT EXISTS coupons_event_id_idx ON coupons (event_id);
CREATE INDEX IF NOT EXISTS coupons_active_idx ON coupons (event_id, active)
    WHERE active = TRUE;

CREATE TABLE IF NOT EXISTS coupon_redemptions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- RESTRICT not CASCADE: deleting a coupon while a cart still references
    -- it would corrupt order history. Soft-delete (active=false) is the way.
    coupon_id           UUID NOT NULL REFERENCES coupons(id) ON DELETE RESTRICT,
    -- One redemption per cart — a buyer can't stack two applies of the same
    -- coupon by re-submitting the form.
    cart_id             UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    status              TEXT NOT NULL CHECK (status IN ('reserved', 'confirmed', 'expired', 'refunded')),
    -- Snapshot of the discount in cents at apply time. We persist this so a
    -- later coupon edit (e.g., bumping the percent) doesn't change what the
    -- buyer paid. Confirmed-at populated on payment webhook.
    applied_value_cents BIGINT NOT NULL,
    reserved_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at        TIMESTAMPTZ,
    UNIQUE (cart_id)
);

CREATE INDEX IF NOT EXISTS coupon_redemptions_coupon_id_idx ON coupon_redemptions (coupon_id);
CREATE INDEX IF NOT EXISTS coupon_redemptions_status_idx ON coupon_redemptions (status);

-- Cart-side reference. We keep the redemption table as the source of truth
-- and mirror just enough on the cart to compute totals without a join. The
-- cart row always sees the current applied discount even when the redemption
-- has been confirmed/refunded — the column reflects what the buyer is being
-- charged.
ALTER TABLE carts ADD COLUMN IF NOT EXISTS coupon_id UUID REFERENCES coupons(id) ON DELETE SET NULL;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS coupon_code TEXT;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS coupon_discount_cents BIGINT NOT NULL DEFAULT 0;
