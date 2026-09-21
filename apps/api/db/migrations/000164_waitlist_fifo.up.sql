-- Each request has its own priority and frozen price. A later comment is a new
-- request; replay protection is the source command, not the buyer/product pair.
DROP INDEX IF EXISTS uq_waitlist_live_entry;
ALTER TABLE waitlist_items
    ADD COLUMN unit_price bigint,
    ADD COLUMN original_quantity integer,
    ADD COLUMN fulfilled_quantity integer NOT NULL DEFAULT 0,
    ADD COLUMN cancelled_quantity integer NOT NULL DEFAULT 0,
    ADD COLUMN source_command text,
    ADD COLUMN queue_sequence bigserial,
    ADD COLUMN price_lot_id uuid REFERENCES cart_item_price_lots(id) ON DELETE SET NULL;

UPDATE waitlist_items wi SET
    unit_price = COALESCE((SELECT ci.unit_price FROM cart_items ci
        WHERE ci.cart_id=wi.cart_id AND ci.product_id=wi.product_id),
        (SELECT cie.unit_price FROM cart_item_events cie WHERE cie.cart_id=wi.cart_id AND cie.product_id=wi.product_id ORDER BY cie.created_at LIMIT 1),0),
    original_quantity = wi.quantity,
    fulfilled_quantity = CASE WHEN wi.status IN ('notified','fulfilled') THEN wi.quantity ELSE 0 END;
WITH ordered AS (
    SELECT id, row_number() OVER (ORDER BY created_at,position,id) AS seq FROM waitlist_items
) UPDATE waitlist_items wi SET queue_sequence=ordered.seq FROM ordered WHERE wi.id=ordered.id;
SELECT setval(pg_get_serial_sequence('waitlist_items','queue_sequence'),
    GREATEST(1,COALESCE((SELECT MAX(queue_sequence) FROM waitlist_items),0)),
    EXISTS(SELECT 1 FROM waitlist_items));
UPDATE waitlist_items wi SET price_lot_id=(
    SELECT MIN(l.id::text)::uuid FROM cart_item_price_lots l
    JOIN cart_items ci ON ci.id=l.cart_item_id
    WHERE ci.cart_id=wi.cart_id AND ci.product_id=wi.product_id
    HAVING COUNT(*)=1
);
ALTER TABLE waitlist_items ALTER COLUMN unit_price SET NOT NULL,
    ALTER COLUMN original_quantity SET NOT NULL;
CREATE UNIQUE INDEX uq_waitlist_source_command ON waitlist_items(source_command,product_id)
    WHERE source_command IS NOT NULL;
CREATE INDEX idx_waitlist_global_fifo ON waitlist_items(product_id,created_at,queue_sequence)
    WHERE status='waiting';

-- Legacy callers still inserting waitlist rows obtain the cart's captured price.
CREATE FUNCTION prepare_waitlist_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.unit_price := COALESCE(NEW.unit_price,(SELECT unit_price FROM cart_items
        WHERE cart_id=NEW.cart_id AND product_id=NEW.product_id));
    IF NEW.unit_price IS NULL AND NEW.status='waiting' THEN
        RAISE EXCEPTION 'a waiting request requires its original cart price';
    END IF;
    NEW.unit_price := COALESCE(NEW.unit_price,0);
    NEW.original_quantity := COALESCE(NEW.original_quantity,NEW.quantity);
    IF NEW.price_lot_id IS NULL THEN
        SELECT l.id INTO NEW.price_lot_id FROM cart_item_price_lots l
        JOIN cart_items ci ON ci.id=l.cart_item_id
        WHERE ci.cart_id=NEW.cart_id AND ci.product_id=NEW.product_id
          AND l.waitlisted_quantity>0 AND l.unit_price=NEW.unit_price
        ORDER BY l.sequence DESC LIMIT 1;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER prepare_waitlist_request BEFORE INSERT ON waitlist_items
    FOR EACH ROW EXECUTE FUNCTION prepare_waitlist_request();

-- A cart quantity edit removes the newest unfulfilled request units first via
-- the price ledger. Reflect those cancellations without touching served units.
CREATE FUNCTION cancel_reduced_waitlist_lot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE remaining integer; entry record; take integer;
BEGIN
    IF TG_OP='DELETE' THEN
        remaining := OLD.waitlisted_quantity;
    ELSE
        remaining := LEAST(GREATEST(OLD.quantity-NEW.quantity,0),
            GREATEST(OLD.waitlisted_quantity-NEW.waitlisted_quantity,0));
    END IF;
    FOR entry IN SELECT id,quantity FROM waitlist_items
        WHERE price_lot_id=OLD.id AND status='waiting'
        ORDER BY created_at DESC,queue_sequence DESC FOR UPDATE LOOP
        EXIT WHEN remaining<=0;
        take := LEAST(entry.quantity,remaining);
        UPDATE waitlist_items SET quantity=quantity-take,
            cancelled_quantity=cancelled_quantity+take,
            status=CASE WHEN quantity=take THEN 'cancelled' ELSE status END,
            cancelled_at=CASE WHEN quantity=take THEN now() ELSE cancelled_at END
        WHERE id=entry.id;
        remaining := remaining-take;
    END LOOP;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER cancel_reduced_waitlist_lot AFTER UPDATE ON cart_item_price_lots
    FOR EACH ROW EXECUTE FUNCTION cancel_reduced_waitlist_lot();
CREATE TRIGGER cancel_deleted_waitlist_lot BEFORE DELETE ON cart_item_price_lots
    FOR EACH ROW EXECUTE FUNCTION cancel_reduced_waitlist_lot();

-- Promoted products now share only their cart's expiration. Historical expired
-- entries stay expired; this migration never restores a previously removed item.
UPDATE waitlist_items SET expires_at=NULL WHERE status='notified';
