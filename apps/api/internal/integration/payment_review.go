package integration

import (
	"context"
	"fmt"
	"time"

	"livecart/apps/api/internal/erp"
)

func (r *Repository) RecordERPFinancialReview(
	ctx context.Context, cartID, storeID string, review erp.PaymentReview,
) error {
	if review.CheckedAt.IsZero() {
		review.CheckedAt = time.Now().UTC()
	}
	result, err := r.pool.Exec(ctx, `INSERT INTO erp_payment_reviews
        (cart_id,external_order_id,reason,paid_cents,order_total_cents,checked_at)
        SELECT c.id,$3,$4,$5,$6,$7 FROM carts c JOIN live_events e ON e.id=c.event_id
        WHERE c.id=$1 AND e.store_id=$2 AND c.external_order_id=$3
        ON CONFLICT(cart_id) DO UPDATE SET external_order_id=EXCLUDED.external_order_id,
          reason=EXCLUDED.reason,paid_cents=EXCLUDED.paid_cents,order_total_cents=EXCLUDED.order_total_cents,
          detected_at=CASE WHEN erp_payment_reviews.resolved_at IS NOT NULL THEN now()
            ELSE erp_payment_reviews.detected_at END,checked_at=EXCLUDED.checked_at,resolved_at=NULL
          WHERE erp_payment_reviews.checked_at<=EXCLUDED.checked_at`,
		cartID, storeID, review.ExternalOrderID, review.Reason, review.PaidCents, review.OrderTotalCents, review.CheckedAt)
	if err != nil {
		return fmt.Errorf("recording ERP financial review: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("ERP financial review binding changed: %w", erp.ErrCartBusy)
	}
	return nil
}

func (r *Repository) ResolveERPFinancialReview(ctx context.Context, cartID, storeID, externalOrderID string, checkedAt time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE erp_payment_reviews r SET resolved_at=now(),checked_at=$4
        FROM carts c JOIN live_events e ON e.id=c.event_id
        WHERE r.cart_id=c.id AND c.id=$1 AND e.store_id=$2 AND c.external_order_id=$3
          AND r.external_order_id=$3 AND r.resolved_at IS NULL AND r.checked_at<=$4`, cartID, storeID, externalOrderID, checkedAt)
	if err != nil {
		return fmt.Errorf("resolving ERP financial review: %w", err)
	}
	return nil
}
