-- One immutable commercial close and one cart deadline replace item timers.
ALTER TABLE live_events
    ADD COLUMN commercial_closed_at timestamptz,
    DROP CONSTRAINT IF EXISTS live_events_waitlist_notified_ttl_minutes_check,
    ADD CONSTRAINT live_events_waitlist_notified_ttl_minutes_check
        CHECK (waitlist_notified_ttl_minutes BETWEEN 0 AND 43200);

ALTER TABLE carts
    ADD COLUMN waitlist_extra_eligible boolean,
    ADD COLUMN deadline_config_base_at timestamptz,
    ADD COLUMN deadline_config_x_minutes integer,
    ADD COLUMN deadline_config_y_minutes integer;

COMMENT ON COLUMN live_events.commercial_closed_at IS
    'Immutable commercial close E; scheduled close uses ends_at even when its worker runs late. NULL on legacy events without evidence.';
COMMENT ON COLUMN carts.waitlist_extra_eligible IS
    'Whether stock was still awaited at E. NULL means the legacy history cannot prove eligibility.';
COMMENT ON COLUMN carts.deadline_config_base_at IS
    'Existing deadline at migration, used only when historical E/eligibility cannot be reconstructed. Never an invented event close.';
COMMENT ON COLUMN live_events.waitlist_notified_ttl_minutes IS
    'Additional cart deadline Y, in minutes (0..43200), granted to carts awaiting stock at commercial close. Never an item TTL.';

-- The transactional event is evidence of closure; updated_at is not. Historical
-- events without this evidence keep E unknown and keep their existing deadline.
UPDATE live_events e
SET commercial_closed_at = LEAST(e.ends_at, proof.closed_at)
FROM (
    SELECT live_event_id, min(created_at) AS closed_at
    FROM event_outbox
    WHERE name = 'event.ended' AND live_event_id IS NOT NULL
    GROUP BY live_event_id
) proof
WHERE proof.live_event_id = e.id AND e.status = 'ended';

-- A still-waiting entry created before E, or an allocation/cancellation after E,
-- proves the cart was waiting at E. Missing history is deliberately left NULL.
UPDATE carts c
SET waitlist_extra_eligible = true
FROM live_events e
WHERE e.id = c.event_id AND e.commercial_closed_at IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM waitlist_items wi
      WHERE wi.cart_id = c.id AND wi.created_at <= e.commercial_closed_at
        AND (wi.status = 'waiting'
             OR wi.notified_at > e.commercial_closed_at
             OR wi.cancelled_at > e.commercial_closed_at)
  );

-- Record the actual granted legacy date and the current config, so X increases
-- are monotonic even when the original close is unknown. A decrease followed by
-- the same increase cannot grant the same minutes twice. No cart is reopened.
UPDATE carts c
SET deadline_config_base_at = c.expires_at,
    deadline_config_x_minutes = CASE WHEN e.close_cart_on_event_end
        THEN COALESCE(e.cart_expiration_minutes, s.cart_expiration_minutes)
        ELSE COALESCE(e.cart_extended_expiration_minutes, s.cart_extended_expiration_minutes) END,
    deadline_config_y_minutes = e.waitlist_notified_ttl_minutes
FROM live_events e JOIN stores s ON s.id = e.store_id
WHERE c.event_id = e.id AND c.expires_at IS NOT NULL;

CREATE FUNCTION capture_commercial_close() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.commercial_closed_at IS NOT NULL THEN
        NEW.commercial_closed_at := OLD.commercial_closed_at;
    ELSIF NEW.status = 'ended' AND OLD.status IS DISTINCT FROM 'ended' THEN
        NEW.commercial_closed_at := LEAST(now(), NEW.ends_at);
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER capture_commercial_close
BEFORE UPDATE ON live_events
FOR EACH ROW EXECUTE FUNCTION capture_commercial_close();

CREATE FUNCTION freeze_waitlist_deadline_eligibility() RETURNS trigger LANGUAGE plpgsql AS $$
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
          AND NOT c.never_expires;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER freeze_waitlist_deadline_eligibility
AFTER UPDATE OF status ON live_events
FOR EACH ROW EXECUTE FUNCTION freeze_waitlist_deadline_eligibility();
