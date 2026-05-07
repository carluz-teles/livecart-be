-- Backfill paid carts that never landed in the ERP so they show up in the
-- new "Precisam atenção" tab. Without this they sit in "Para despachar"
-- with no external_order_id, invisible to the merchant until they click
-- into each one — exactly the silent inconsistency the retry flow exists
-- to surface.
--
-- Criteria for backfill (intentionally conservative):
--   - payment_status = 'paid' and external_order_id IS NULL
--     (paid customer, no ERP order)
--   - erp_finalisation_status still at column default 'pending'
--     (don't clobber rows that have already moved to 'done' or 'failed')
--   - paid_at is more than 5 minutes ago
--     (avoids racing in-flight finalisations the migration could run
--     concurrently with on first deploy)
--   - paid_at is within the last 60 days
--     (older than that, support handles manually — auto-finalisation
--     wasn't always enforced and noise risk outweighs the value)
--   - the store currently has an active ERP integration of any provider
--     (no point flagging "ERP failed" on a store that never had one;
--     when more providers land, this migration's intent stays valid
--     because the WHERE checks type='erp' generically)
--
-- erp_payment_snapshot is reconstructed from cart columns so the retry
-- endpoint has enough to replay the order creation. Missing fields
-- (fee_amount_cents, money_release_date, installments) default to safe
-- values — installments=1 collapses the schedule into a single parcela on
-- paid_at, which is correct for Pix/boleto and a fair fallback for
-- single-installment cards. Multi-installment retries from the backfill
-- will go through as a single parcela; the merchant can still complete
-- the order in Tiny manually if they need the original split.

UPDATE carts c
SET
    erp_finalisation_status = 'failed',
    erp_last_error          = 'Pedido pago sem confirmação no ERP. Importado pelo backfill — abra e tente novamente para reenviar.',
    erp_last_attempt_at     = COALESCE(c.paid_at, c.created_at),
    erp_attempts_count      = c.erp_attempts_count + 1,
    erp_payment_snapshot    = jsonb_build_object(
        'payment_id', COALESCE(c.checkout_id, ''),
        'status', 'paid',
        'amount', COALESCE((
            SELECT SUM(ci.unit_price * ci.quantity)::BIGINT
            FROM cart_items ci
            WHERE ci.cart_id = c.id
        ), 0),
        'paid_at', c.paid_at,
        'external_reference', c.id::text,
        'payment_method', COALESCE(c.payment_method, ''),
        'installments', 1
    )
FROM live_events e
WHERE c.event_id = e.id
  AND c.payment_status = 'paid'
  AND (c.external_order_id IS NULL OR c.external_order_id = '')
  AND c.erp_finalisation_status = 'pending'
  AND c.paid_at IS NOT NULL
  AND c.paid_at < now() - interval '5 minutes'
  AND c.paid_at > now() - interval '60 days'
  AND EXISTS (
      SELECT 1 FROM integrations i
      WHERE i.store_id = e.store_id
        AND i.type = 'erp'
        AND i.status = 'active'
  );
