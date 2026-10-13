package integration

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

type tinyCheckoutRecovery struct {
	CartID   string
	StoreID  string
	Attempts int
}

// Only locally journaled, paid checkouts are recovered. This is not an import
// of historical ERP orders, nor a replay of payment or notification events.
func (r *Repository) claimTinyCheckoutRecovery(ctx context.Context) ([]tinyCheckoutRecovery, error) {
	rows, err := r.pool.Query(ctx, `WITH candidates AS (
        SELECT t.id,e.store_id FROM tiny_checkout_operations t
        JOIN carts c ON c.id=t.cart_id JOIN live_events e ON e.id=c.event_id
        JOIN orders o ON o.cart_id=c.id AND o.store_id=e.store_id
        JOIN order_payments p ON p.order_id=o.id
        WHERE t.recovery_after<=now() AND t.updated_at<now()-interval '10 minutes'
          AND c.joined_to_cart_id IS NULL AND c.payment_status='paid'
          AND NOT c.payment_review_required AND c.status NOT IN ('expired','cancelled')
          AND o.status NOT IN ('cancelled','refunded')
          AND c.erp_order_state IN ('open','confirmed')
          AND c.external_order_id IN (t.source_order_id,t.target_order_id)
          AND (NOT t.completed OR p.erp_finalisation_status IN ('failed','pending'))
          AND (p.erp_last_attempt_at IS NULL OR p.erp_last_attempt_at<now()-interval '10 minutes')
          AND t.integration_id=(SELECT i.id FROM integrations i
              WHERE i.store_id=e.store_id AND i.type='erp' AND i.status='active'
              ORDER BY i.created_at,i.id LIMIT 1)
          AND NOT EXISTS(SELECT 1 FROM tiny_checkout_operations newer
              WHERE newer.cart_id=t.cart_id AND (newer.created_at,newer.id)>(t.created_at,t.id))
        ORDER BY t.recovery_after,t.id LIMIT 10 FOR UPDATE OF t SKIP LOCKED
    ) UPDATE tiny_checkout_operations t SET recovery_attempts=t.recovery_attempts+1,
        recovery_after=now()+make_interval(mins=>LEAST(360,10*power(2,LEAST(t.recovery_attempts,6)))::integer)
      FROM candidates c WHERE t.id=c.id
      RETURNING t.cart_id::text,c.store_id::text,t.recovery_attempts`)
	if err != nil {
		return nil, fmt.Errorf("claiming pending Tiny checkouts: %w", err)
	}
	defer rows.Close()
	var pending []tinyCheckoutRecovery
	for rows.Next() {
		var item tinyCheckoutRecovery
		if err := rows.Scan(&item.CartID, &item.StoreID, &item.Attempts); err != nil {
			return nil, fmt.Errorf("reading pending Tiny checkout: %w", err)
		}
		pending = append(pending, item)
	}
	return pending, rows.Err()
}

func (s *Service) RecoverTinyPaidCheckouts(ctx context.Context) {
	pending, err := s.repo.claimTinyCheckoutRecovery(ctx)
	if err != nil {
		s.logger.Error("listing pending Tiny checkouts", zap.Error(err))
		return
	}
	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}
		// Retry reuses the cart lock, current payment snapshot and operation
		// marker, including adoption after a lost response to an earlier POST.
		err := s.RetryERPFinalisation(ctx, item.CartID, item.StoreID)
		if err != nil {
			s.logger.Warn("Tiny paid checkout still requires reconciliation",
				zap.String("cart_id", item.CartID), zap.String("store_id", item.StoreID),
				zap.Int("recovery_attempts", item.Attempts), zap.Error(err))
		}
	}
}
