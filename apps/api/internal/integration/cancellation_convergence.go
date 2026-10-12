package integration

import (
	"context"

	"livecart/apps/api/internal/erp"
)

// Do not resolve joined children to their owner here: this transition belongs
// to the exact sale for which the remote operation was made. The predicates
// remain authoritative even if payment, reopening or rebinding races the GET.
func (r *Repository) TransitionCartCancellation(ctx context.Context, change erp.CancellationTransition) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE carts c SET erp_order_state=$6,erp_op_resting_state=NULL
        WHERE c.id=$1 AND c.store_id=$2 AND c.external_order_id=$3
          AND c.erp_order_state=$5 AND c.status=$7 AND c.joined_to_cart_id IS NULL
          AND (COALESCE(c.erp_order_status,'')=$10 OR c.erp_order_status='cancelado')
          AND NOT c.purchase_closed AND c.paid_amount_cents=0
          AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
          AND EXISTS(SELECT 1 FROM integrations i WHERE i.id=$4 AND i.store_id=c.store_id
            AND i.type='erp' AND i.status='active')
          AND (NOT $8::boolean OR (c.status='expired' AND c.expires_at<=now()
            AND c.expires_at IS NOT DISTINCT FROM $9::timestamptz
            AND NOT c.never_expires AND NOT c.payment_review_required
            AND NOT is_active_vip(c.store_id,c.platform_handle)
            AND (erp_order_accepts_items(c.erp_order_status) OR c.erp_order_status='cancelado')))`,
		change.CartID, change.StoreID, change.ExternalOrderID, change.IntegrationID,
		change.From, change.To, change.CartStatus, change.ExpiryOnly, change.ExpiresAt, change.OrderStatus,
	)
	return tag.RowsAffected() > 0, err
}
