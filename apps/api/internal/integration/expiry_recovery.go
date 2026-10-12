package integration

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// RecoverRecentCartExpiries repairs recently lost ETA tasks, including retries
// exhausted during a short outage. Older legacy carts require reconciliation:
// a sweep must not blindly cancel sales manually settled outside LiveCart.
func (s *Service) RecoverRecentCartExpiries(ctx context.Context) {
	defer s.recoverConfirmedExpiredCancellations(ctx)
	rows, err := s.repo.pool.Query(ctx, `SELECT c.id::text FROM carts c
        LEFT JOIN carts host ON host.id=c.joined_to_cart_id
        WHERE c.status IN ('active','checkout')
          AND c.expires_at<now()-interval '30 seconds' AND c.expires_at>now()-interval '24 hours'
          AND NOT c.never_expires AND NOT is_active_vip(c.store_id, c.platform_handle) AND NOT c.purchase_closed AND NOT c.payment_review_required
          AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
          AND erp_order_accepts_items(c.erp_order_status)
          AND (host.id IS NULL OR (NOT host.never_expires
            AND NOT is_active_vip(host.store_id, host.platform_handle) AND NOT host.purchase_closed AND NOT host.payment_review_required
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

// Unlike the ETA recovery above, this includes historical expired carts. It
// never sends a cancellation: a fresh read must prove that the exact bound sale
// is already cancelled/absent before changing local state. Rotate a bounded
// batch so an unavailable account cannot monopolise the recovery window.
func (s *Service) recoverConfirmedExpiredCancellations(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := s.repo.pool.Query(ctx, `SELECT c.id::text,c.store_id::text FROM carts c
        WHERE c.status='expired' AND c.expires_at<=now() AND c.erp_order_state='open'
          AND c.erp_order_status='cancelado' AND COALESCE(c.external_order_id,'')<>''
          AND c.joined_to_cart_id IS NULL AND NOT c.purchase_closed
          AND c.paid_amount_cents=0 AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
          AND NOT c.never_expires AND NOT c.payment_review_required
          AND NOT is_active_vip(c.store_id,c.platform_handle)
          AND EXISTS(SELECT 1 FROM integrations i WHERE i.store_id=c.store_id
            AND i.type='erp' AND i.provider='tiny' AND i.status='active')
        ORDER BY md5(c.id::text || date_trunc('minute',now())::text) LIMIT 5`)
	if err != nil {
		s.logger.Error("listing confirmed expired cancellations", zap.Error(err))
		return
	}
	type candidate struct{ cart, store string }
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.cart, &item.store); err != nil {
			break
		}
		candidates = append(candidates, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Error("reading confirmed expired cancellations", zap.Error(err))
		return
	}
	for _, item := range candidates {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := s.erpStock().ReconcileExpiredCartCancellation(attempt, item.cart, item.store)
		cancel()
		if err != nil {
			s.logger.Warn("expired cancellation confirmation deferred",
				zap.String("cart_id", item.cart), zap.String("store_id", item.store), zap.Error(err))
		}
	}
}
