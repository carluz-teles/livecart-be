CREATE TABLE cart_erp_item_blocks (
    cart_id uuid PRIMARY KEY REFERENCES carts(id) ON DELETE CASCADE,
    integration_id uuid NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    external_order_id text NOT NULL CHECK (external_order_id <> ''),
    blocked_at timestamptz NOT NULL DEFAULT now(),
    retry_at timestamptz NOT NULL DEFAULT now() + interval '30 minutes'
);

-- A block belongs to one ERP order. Rebinding, reconciling or removing its
-- pending lines must not leave a different purchase blocked.
CREATE VIEW active_cart_erp_item_blocks AS
SELECT b.*, e.store_id FROM cart_erp_item_blocks b
JOIN carts c ON c.id=b.cart_id AND c.external_order_id=b.external_order_id
JOIN live_events e ON e.id=c.event_id
WHERE c.joined_to_cart_id IS NULL AND c.status NOT IN ('cancelled','expired')
  AND b.integration_id=(SELECT i.id FROM integrations i
      WHERE i.store_id=e.store_id AND i.type='erp' AND i.status='active'
      ORDER BY i.created_at,i.id LIMIT 1)
  AND EXISTS (SELECT 1 FROM cart_items ci JOIN carts member ON member.id=ci.cart_id
      WHERE COALESCE(member.joined_to_cart_id,member.id)=c.id
      AND member.status NOT IN ('cancelled','expired')
      AND ci.quantity>ci.waitlisted_quantity
      AND (ci.erp_pending_since IS NOT NULL
          OR COALESCE(ci.erp_confirmed_quantity,-1)<ci.quantity-ci.waitlisted_quantity));

ALTER TABLE tiny_checkout_operations
    ADD COLUMN recovery_after timestamptz NOT NULL DEFAULT now() + interval '10 minutes',
    ADD COLUMN recovery_attempts integer NOT NULL DEFAULT 0;
CREATE INDEX tiny_checkout_operations_recovery ON tiny_checkout_operations(recovery_after);
