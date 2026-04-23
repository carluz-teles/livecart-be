-- Add free_shipping flag to live_events. When true, the shipping cost charged
-- to the customer is zero even though the real cost is still recorded on the
-- cart so the merchant can see what the shipment will cost them.

ALTER TABLE live_events
    ADD COLUMN IF NOT EXISTS free_shipping BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN live_events.free_shipping IS 'When true, charge the customer R$0 shipping regardless of the quoted price (real cost is still recorded on the cart)';
