package integration

import (
	"context"
	"fmt"

	"livecart/apps/api/internal/integration/providers"
)

// IsLegacyCartWithoutInvoiceWorkflow identifies the September history reviewed
// in the October audit. The fixed boundary is intentional: a new missing order
// must never become a benign omission just because it gets older. An old cart
// alone is insufficient; payment provenance and absence of local work must agree.
// This is only a read classification, never permission to materialize or pay it.
func (r *Repository) IsLegacyCartWithoutInvoiceWorkflow(
	ctx context.Context,
	storeID, cartID, externalOrderID string,
) (bool, error) {
	store, err := parseUUID(storeID)
	if err != nil {
		return false, fmt.Errorf("parsing invoice history store: %w", err)
	}
	cart, err := parseUUID(cartID)
	if err != nil {
		return false, fmt.Errorf("parsing invoice history cart: %w", err)
	}

	// erp_op_started_at is retained after a finished operation. Price lots can
	// also change through ERP reflection. Neither alone establishes a local
	// invoice workflow; checkout, payment, merchant edits and logistics do.
	var legacy bool
	err = r.pool.QueryRow(ctx, `
WITH history AS (SELECT timestamptz '2026-10-01 00:00:00-03' AS boundary)
SELECT EXISTS (
    SELECT 1 FROM carts c
    JOIN live_events e ON e.id=c.event_id AND e.store_id=c.store_id
    CROSS JOIN history h
    WHERE c.id=$1 AND c.store_id=$2 AND c.external_order_id=$3 AND $3<>''
      AND c.created_at<h.boundary AND c.paid_at<h.boundary
      AND c.payment_status='paid' AND c.paid_amount_cents>0
      AND c.status IN ('active','checkout')
      AND e.status='ended' AND e.ends_at<h.boundary
      AND c.erp_order_state IN ('open','confirmed')
      AND c.erp_order_status IN ('aprovado','faturado','preparando_envio','pronto_envio','enviado','entregue')
      AND c.erp_items_retry_at IS NULL AND NOT c.payment_review_required
      AND c.joined_to_cart_id IS NULL
      AND (c.cancellation_reverted_at IS NULL OR c.cancellation_reverted_at<h.boundary)
      AND NOT EXISTS (SELECT 1 FROM carts child WHERE child.joined_to_cart_id=c.id)
      AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.cart_id=c.id)
      AND EXISTS (SELECT 1 FROM cart_payments p WHERE p.cart_id=c.id)
      AND NOT EXISTS (
          SELECT 1 FROM cart_payments p WHERE p.cart_id=c.id
            AND (p.method IS DISTINCT FROM $4 OR p.checkout_id<>'erp-'||c.external_order_id
              OR p.amount_cents<=0 OR p.paid_at>=h.boundary OR p.created_at>=h.boundary)
      )
      AND c.paid_amount_cents=(SELECT sum(p.amount_cents) FROM cart_payments p WHERE p.cart_id=c.id)
      AND (COALESCE(c.checkout_id,'')='' OR c.checkout_id='erp-'||c.external_order_id)
      AND c.payment_integration_id IS NULL AND COALESCE(c.pix_charge_id,'')=''
      AND NOT EXISTS (SELECT 1 FROM payment_attempts p WHERE p.cart_id=c.id)
      AND NOT EXISTS (
          SELECT 1 FROM tiny_checkout_operations op WHERE op.cart_id=c.id
            AND (NOT op.completed OR op.updated_at>=h.boundary)
      )
      AND (c.shipping_address IS NULL OR c.shipping_address IN ('{}'::jsonb,'null'::jsonb))
      AND COALESCE(c.shipping_service_id,'')='' AND COALESCE(c.shipping_provider,'')=''
      AND NOT EXISTS (SELECT 1 FROM shipments s WHERE s.cart_id=c.id)
      AND NOT EXISTS (
          SELECT 1 FROM erp_payment_reviews pr WHERE pr.cart_id=c.id
            AND (pr.resolved_at IS NULL OR pr.checked_at>=h.boundary)
      )
      AND NOT EXISTS (SELECT 1 FROM cart_items ci WHERE ci.cart_id=c.id AND ci.erp_pending_since IS NOT NULL)
      AND NOT EXISTS (
          SELECT 1 FROM cart_erp_edits edit WHERE edit.cart_id=c.id
            AND (edit.revision>edit.synced_revision OR edit.queued_at>=h.boundary
              OR edit.blocked_at IS NOT NULL OR edit.lease_until IS NOT NULL)
      )
      AND NOT EXISTS (
          SELECT 1 FROM cart_erp_edit_requests edit WHERE edit.cart_id=c.id AND edit.created_at>=h.boundary
      )
      AND NOT EXISTS (SELECT 1 FROM cart_mutations m WHERE m.cart_id=c.id AND m.created_at>=h.boundary)
      AND NOT EXISTS (SELECT 1 FROM cart_item_events a WHERE a.cart_id=c.id AND a.created_at>=h.boundary)
      AND NOT EXISTS (
          SELECT 1 FROM cart_item_events a JOIN live_comment_work w USING (platform_comment_id)
          WHERE a.cart_id=c.id AND w.completed_at IS NULL
      )
)`, cart, store, externalOrderID, providers.PaymentMethodERPManual).Scan(&legacy)
	if err != nil {
		return false, fmt.Errorf("classifying invoice history: %w", err)
	}
	return legacy, nil
}
