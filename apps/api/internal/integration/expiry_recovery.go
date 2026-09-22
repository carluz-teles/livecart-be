package integration

import (
	"context"

	"go.uber.org/zap"
)

// RecoverRecentCartExpiries repairs recently lost ETA tasks, including retries
// exhausted during a short outage. Older legacy carts require reconciliation:
// a sweep must not blindly cancel sales manually settled outside LiveCart.
func (s *Service) RecoverRecentCartExpiries(ctx context.Context) {
	rows, err := s.repo.pool.Query(ctx, `SELECT c.id::text FROM carts c
        LEFT JOIN carts host ON host.id=c.joined_to_cart_id
        WHERE c.status IN ('active','checkout')
          AND c.expires_at<now()-interval '30 seconds' AND c.expires_at>now()-interval '24 hours'
          AND NOT c.never_expires AND NOT c.purchase_closed AND NOT c.payment_review_required
          AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
          AND erp_order_accepts_items(c.erp_order_status)
          AND (host.id IS NULL OR (NOT host.purchase_closed AND NOT host.payment_review_required
            AND host.status IN ('active','checkout') AND erp_order_accepts_items(host.erp_order_status)
            AND COALESCE(host.payment_status,'pending') NOT IN ('paid','refunded')))
        ORDER BY c.expires_at,c.id LIMIT 50`)
	if err != nil {
		s.logger.Error("listing recent cart expiries for recovery", zap.Error(err))
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Error("reading cart expiry recovery batch", zap.Error(err))
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.RunScheduledExpiry(ctx, id); err != nil {
			s.logger.Warn("cart expiry recovery deferred", zap.String("cart_id", id), zap.Error(err))
		}
	}
}
