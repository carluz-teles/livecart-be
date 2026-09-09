package checkout

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/logger"
)

type merchantEditWork struct {
	cartID, storeID, token, owner string
	revision                      int64
	queuedAt                      time.Time
	attempts                      int
}

// Recovery reads the database, not a fire-and-forget HTTP goroutine. Claims
// expire after the operation deadline so another replica can resume a crash.
func (s *Service) RecoverMerchantEdits(ctx context.Context) {
	owner := uuid.NewString()
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
        SELECT c.id FROM carts c JOIN cart_erp_edits w ON w.cart_id=c.id
        WHERE w.revision>w.synced_revision AND w.next_attempt_at<=now()
        AND (w.lease_until IS NULL OR w.lease_until<now())
        ORDER BY w.next_attempt_at LIMIT 5 FOR UPDATE OF c SKIP LOCKED
    ), claimed AS (
        UPDATE cart_erp_edits w SET lease_owner=$1,lease_until=now()+interval '3 minutes',attempts=attempts+1
        FROM candidates c WHERE c.id=w.cart_id AND w.revision>w.synced_revision
        AND w.next_attempt_at<=now() AND (w.lease_until IS NULL OR w.lease_until<now()) RETURNING w.*
    ) SELECT w.cart_id::text,COALESCE(c.store_id,e.store_id)::text,c.token,w.revision,w.queued_at,w.attempts
      FROM claimed w JOIN carts c ON c.id=w.cart_id JOIN live_events e ON e.id=c.event_id`, owner)
	if err != nil {
		s.logger.Error("claiming merchant edits", zap.Error(err))
		return
	}
	work := []merchantEditWork{}
	for rows.Next() {
		w := merchantEditWork{owner: owner}
		if err = rows.Scan(&w.cartID, &w.storeID, &w.token, &w.revision, &w.queuedAt, &w.attempts); err != nil {
			break
		}
		work = append(work, w)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Error("reading merchant edits", zap.Error(err))
		return
	}
	// Each claim has its own deadline immediately, including time awaiting the
	// shared account budget. Independent carts cannot consume another's lease.
	done := make(chan struct{}, len(work))
	for _, w := range work {
		go func(w merchantEditWork) {
			defer func() { done <- struct{}{} }()
			opCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			started := time.Now()
			err := s.syncMerchantEdit(opCtx, w)
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stop()
			if err != nil {
				_, saveErr := s.pool.Exec(cleanup, `UPDATE cart_erp_edits SET lease_owner=NULL,lease_until=NULL,
                    last_error=$3,next_attempt_at=now()+make_interval(secs=>LEAST(900,5*power(2,LEAST(attempts,7)))::double precision)
                    WHERE cart_id=$1 AND lease_owner=$2`, w.cartID, w.owner, err.Error())
				s.logger.Warn("merchant edit sync deferred", zap.String("cart_id", w.cartID), zap.String("store_id", w.storeID),
					zap.Int("attempt", w.attempts), zap.Duration("queue_wait", started.Sub(w.queuedAt)),
					zap.Duration("sync_duration", time.Since(started)), zap.Error(err), zap.NamedError("persist_error", saveErr))
				return
			}
			s.logger.Info("merchant edit sync completed", zap.String("cart_id", w.cartID), zap.String("store_id", w.storeID),
				zap.Int64("revision", w.revision), zap.Int("attempt", w.attempts),
				zap.Duration("queue_wait", started.Sub(w.queuedAt)), zap.Duration("sync_duration", time.Since(started)))
		}(w)
	}
	for range work {
		<-done
	}
}

func (s *Service) syncMerchantEdit(ctx context.Context, w merchantEditWork) error {
	cart, err := s.repo.GetCartByToken(ctx, w.token)
	if err != nil {
		return err
	}
	ctx = logger.WithLiveEvent(logger.WithStore(ctx, cart.StoreID, cart.StoreSlug), cart.EventID)
	if cart.Status == "cancelled" || cart.Status == "expired" {
		var state string
		if err := s.pool.QueryRow(ctx, `SELECT erp_order_state FROM carts WHERE id=$1`, w.cartID).Scan(&state); err != nil {
			return err
		}
		if state != "cancelled" && state != "none" {
			return fmt.Errorf("aguardando confirmação do cancelamento no ERP")
		}
	} else {
		var blocked bool
		if err := s.pool.QueryRow(ctx, `SELECT payment_review_required OR COALESCE(payment_status,'') IN ('paid','refunded') FROM carts WHERE id=$1`, w.cartID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return fmt.Errorf("pagamento recebido durante a edição; pedido requer conferência")
		}
		// Failure to cancel the old quote remains durable; do not announce a
		// completed edit while an obsolete PIX is still active.
		if err := s.invalidatePendingPix(ctx, cart); err != nil {
			return fmt.Errorf("invalidating previous PIX: %w", err)
		}
		if s.couponLifecycle != nil {
			if err := s.couponLifecycle.OnCartMutated(ctx, cart.ID); err != nil {
				return err
			}
		}
		owned, err := cartedit.PendingProducts(ctx, s.pool, w.cartID)
		if err != nil {
			return err
		}
		ctx = erp.WithEditedProducts(ctx, owned)
		if err := s.merchantEditERP.MutateERPOrderItems(ctx, w.cartID, w.storeID); err != nil {
			return err
		}
	}
	products, err := s.finishMerchantEdit(ctx, w)
	if err != nil {
		return err
	}
	// Promotion happens only after the ERP confirms the release and its local
	// credit commits. The existing waitlist recovery handles interrupted work.
	for _, p := range products {
		s.merchantEditERP.ProcessWaitlistForProduct(ctx, cart.EventID, p, cart.StoreID)
	}
	return nil
}

func (s *Service) finishMerchantEdit(ctx context.Context, w merchantEditWork) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var eventID string
	if err := tx.QueryRow(ctx, `SELECT event_id::text FROM carts WHERE id=$1 FOR UPDATE`, w.cartID).Scan(&eventID); err != nil {
		return nil, err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_owner=$2::uuid AND revision=$3,false) FROM cart_erp_edits WHERE cart_id=$1 FOR UPDATE`,
		w.cartID, w.owner, w.revision).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("merchant edit claim changed before acknowledgement")
	}
	rows, err := tx.Query(ctx, `SELECT r.product_id::text,SUM(r.retained_quantity)::int FROM cart_erp_edit_requests r
        JOIN cart_erp_edits w ON w.cart_id=r.cart_id WHERE r.cart_id=$1 AND r.revision>w.synced_revision
        AND r.revision<=$2 GROUP BY r.product_id ORDER BY r.product_id`, w.cartID, w.revision)
	if err != nil {
		return nil, err
	}
	type release struct {
		id  string
		qty int
	}
	releases := []release{}
	for rows.Next() {
		var r release
		if err = rows.Scan(&r.id, &r.qty); err != nil {
			break
		}
		releases = append(releases, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	products := []string{}
	for _, r := range releases {
		if r.qty < 0 {
			return nil, fmt.Errorf("negative retained stock for product %s", r.id)
		}
		if _, err := tx.Exec(ctx, `UPDATE products SET stock=stock+$2,erp_seq=erp_seq+1,updated_at=now() WHERE id=$1`, r.id, r.qty); err != nil {
			return nil, err
		}
		if r.qty > 0 {
			key := fmt.Sprintf("%s:%d:%s", w.cartID, w.revision, r.id)
			if err := emitMerchantStockEvent(ctx, s.repo.q.WithTx(tx), events.StockReleased, key, w.cartID, eventID, r.id, r.qty); err != nil {
				return nil, err
			}
			products = append(products, r.id)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE cart_erp_edits SET synced_revision=$3,lease_owner=NULL,lease_until=NULL,
        last_error=NULL,attempts=0 WHERE cart_id=$1 AND lease_owner=$2`, w.cartID, w.owner, w.revision); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return products, nil
}
