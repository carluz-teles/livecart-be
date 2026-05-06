-- Add pix_discount_percent flag to live_events. When > 0, the cart total is
-- reduced by this percentage at checkout when the buyer pays with Pix. This is
-- independent from coupons and stacks with them.

ALTER TABLE live_events
    ADD COLUMN IF NOT EXISTS pix_discount_percent INTEGER NOT NULL DEFAULT 0
        CHECK (pix_discount_percent >= 0 AND pix_discount_percent <= 100);

COMMENT ON COLUMN live_events.pix_discount_percent IS 'Discount percent applied at checkout when the buyer pays with Pix (0-100). 0 disables the feature.';
