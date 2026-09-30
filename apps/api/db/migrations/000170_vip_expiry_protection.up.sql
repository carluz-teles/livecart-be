-- Membership protects the purchase even when cart consolidation has failed.
CREATE FUNCTION is_active_vip(p_store_id uuid, p_handle text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM vip_handles v
        WHERE v.store_id = p_store_id
          AND v.platform_handle = lower(ltrim(btrim(p_handle), '@'))
          AND v.removed_at IS NULL
    );
$$;

CREATE OR REPLACE FUNCTION freeze_waitlist_deadline_eligibility() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    normal_minutes integer;
BEGIN
    IF NEW.commercial_closed_at IS NOT NULL
       AND OLD.commercial_closed_at IS NULL THEN
        -- Serialize the snapshot with payment/promotion before reading the queue.
        PERFORM id FROM carts WHERE event_id = NEW.id ORDER BY id FOR UPDATE;
        UPDATE carts c
        SET waitlist_extra_eligible = EXISTS (
            SELECT 1 FROM waitlist_items wi
            WHERE wi.cart_id = c.id AND wi.created_at <= NEW.commercial_closed_at
              AND (wi.status = 'waiting'
                   OR wi.notified_at > NEW.commercial_closed_at
                   OR wi.cancelled_at > NEW.commercial_closed_at)
        )
        WHERE c.event_id = NEW.id AND c.waitlist_extra_eligible IS NULL;

        SELECT CASE WHEN NEW.close_cart_on_event_end
            THEN COALESCE(NEW.cart_expiration_minutes, s.cart_expiration_minutes)
            ELSE COALESCE(NEW.cart_extended_expiration_minutes, s.cart_extended_expiration_minutes) END
        INTO normal_minutes FROM stores s WHERE s.id = NEW.store_id;
        -- Publish E, eligibility and deadline together. Keep status active so
        -- FinalizeCartsByEvent still emits the existing checkout-armed events.
        UPDATE carts c
        SET expires_at = GREATEST(c.expires_at,
            NEW.commercial_closed_at + make_interval(mins => normal_minutes
            + CASE WHEN c.waitlist_extra_eligible IS TRUE THEN NEW.waitlist_notified_ttl_minutes ELSE 0 END))
        WHERE c.event_id = NEW.id AND c.status IN ('active', 'checkout')
          AND c.payment_status IS DISTINCT FROM 'paid'
          AND c.payment_status IS DISTINCT FROM 'refunded'
          AND NOT c.never_expires
          AND NOT is_active_vip(c.store_id, c.platform_handle);
    END IF;
    RETURN NULL;
END;
$$;

-- Clear only deadlines: do not merge ERP orders or change inventory. Setting
-- never_expires on every cart would violate the single eternal-cart index.
UPDATE carts c SET expires_at = NULL
WHERE c.status IN ('pending','active','checkout')
  AND c.payment_status IS DISTINCT FROM 'paid'
  AND c.payment_status IS DISTINCT FROM 'refunded'
  AND NOT c.purchase_closed
  AND c.expires_at IS NOT NULL
  AND is_active_vip(c.store_id, c.platform_handle);
