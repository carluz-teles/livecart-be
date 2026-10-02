package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

type merchantEditWork struct {
	cartID, storeID, token, owner string
	revision                      int64
	queuedAt                      time.Time
	attempts                      int
	purchase                      []byte
}

var errMerchantEditPaymentReview = errors.New("pagamento recebido durante a edição; pedido requer conferência")
var errCartEditReconciliation = cartedit.ErrReconciliation

// Recovery reads the database, not a fire-and-forget HTTP goroutine. Claims
// expire after the operation deadline so another replica can resume a crash.
func (s *Service) RecoverMerchantEdits(ctx context.Context) {
	owner := uuid.NewString()
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
        SELECT c.id FROM carts c JOIN cart_erp_edits w ON w.cart_id=c.id
        WHERE w.revision>w.synced_revision AND w.next_attempt_at<=now()
        AND (w.blocked_at IS NULL OR (c.status IN ('cancelled','expired') AND c.erp_order_state IN ('cancelled','none')))
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
			s.runCartEditAttempt(opCtx, w)
		}(w)
	}
	for range work {
		<-done
	}
}

// Both HTTP and background recovery use the same lease and failure cleanup.
func (s *Service) runCartEditAttempt(ctx context.Context, w merchantEditWork) {
	started := time.Now()
	err := s.syncMerchantEdit(ctx, w)
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err != nil {
		if errors.Is(err, erp.ErrPedidoFaturado) || errors.Is(err, erp.ErrCancellationUnconfirmed) || errors.Is(err, errMerchantEditPaymentReview) || errors.Is(err, errCartEditReconciliation) {
			saved, saveErr := s.pool.Exec(cleanup, `UPDATE cart_erp_edits SET lease_owner=NULL,lease_until=NULL,
						blocked_at=COALESCE(blocked_at,now()),last_error=$3 WHERE cart_id=$1 AND lease_owner=$2`,
				w.cartID, w.owner, err.Error())
			if saveErr != nil {
				s.logger.Error("saving merchant edit reconciliation", zap.String("cart_id", w.cartID), zap.Error(saveErr))
				return
			}
			if saved.RowsAffected() == 0 {
				return // Another worker owns the current revision.
			}
			s.logger.Info("merchant edit awaits reconciliation", zap.String("cart_id", w.cartID),
				zap.String("store_id", w.storeID), zap.Int64("revision", w.revision), zap.Error(err))
			return
		}
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
}

func (s *Service) trySyncCartEdit(ctx context.Context, token string) {
	attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	w := merchantEditWork{owner: uuid.NewString()}
	err := s.pool.QueryRow(attempt, `WITH candidate AS (
 SELECT host.id FROM carts origin JOIN carts host ON host.id=COALESCE(origin.joined_to_cart_id,origin.id)
 JOIN cart_erp_edits w ON w.cart_id=host.id WHERE origin.token=$2
 AND w.revision>w.synced_revision AND w.blocked_at IS NULL AND (w.attempts=0 OR w.next_attempt_at<=now())
 AND (w.lease_until IS NULL OR w.lease_until<now()) FOR UPDATE OF host SKIP LOCKED
 ), claimed AS (
 UPDATE cart_erp_edits w SET lease_owner=$1,lease_until=now()+interval '3 minutes',attempts=attempts+1
 FROM candidate c WHERE c.id=w.cart_id AND w.revision>w.synced_revision
 AND (w.lease_until IS NULL OR w.lease_until<now()) RETURNING w.*)
 SELECT w.cart_id::text,COALESCE(c.store_id,e.store_id)::text,c.token,w.revision,w.queued_at,w.attempts
 FROM claimed w JOIN carts c ON c.id=w.cart_id JOIN live_events e ON e.id=c.event_id`, w.owner, token).
		Scan(&w.cartID, &w.storeID, &w.token, &w.revision, &w.queuedAt, &w.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		s.logger.Warn("inline cart edit remains queued", zap.Error(err))
		return
	}
	s.runCartEditAttempt(attempt, w)
}

// Compare purchase lifecycle, not ERP operation state: our own grid write
// changes the latter. A child cancellation during HTTP must cause a new pass.
func cartEditPurchaseSnapshot(ctx context.Context, db cartedit.Reader, cartID string) ([]byte, error) {
	var snapshot []byte
	err := db.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object('id',id,'host',joined_to_cart_id,
 'status',status,'payment',payment_status,'paid',paid_amount_cents,'review',payment_review_required,
 'closed',purchase_closed,'erpStatus',erp_order_status) ORDER BY id) FROM carts WHERE COALESCE(joined_to_cart_id,id)=$1`, cartID).Scan(&snapshot)
	return snapshot, err
}

// The journal belongs to the purchase that accepted it. A detached origin must
// never be acknowledged against the former host's unrelated ERP grid.
func validateCartEditOrigins(ctx context.Context, db cartedit.Reader, w merchantEditWork) error {
	var changed bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_erp_edit_requests r
 JOIN cart_erp_edits w ON w.cart_id=r.cart_id LEFT JOIN carts origin
 ON origin.id=COALESCE(NULLIF(r.request->>'originCartId','')::uuid,r.cart_id)
 WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND r.revision<=$2
 AND (origin.id IS NULL OR COALESCE(origin.joined_to_cart_id,origin.id)<>r.cart_id))`, w.cartID, w.revision).Scan(&changed)
	if err != nil {
		return err
	}
	if changed {
		return fmt.Errorf("purchase membership changed after accepting edit: %w", errCartEditReconciliation)
	}
	return nil
}

func (s *Service) cartEditExecution(ctx context.Context, w merchantEditWork) (cartEditExecution, error) {
	return cartedit.ReadExecution(ctx, s.pool, w.cartID, w.revision)
}

func (s *Service) refreshCartEditQuotes(ctx context.Context, w merchantEditWork) error {
	rows, err := s.pool.Query(ctx, `SELECT c.token FROM carts c WHERE c.id=$1 OR c.id IN (
 SELECT COALESCE(NULLIF(r.request->>'originCartId','')::uuid,r.cart_id)
 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
 WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND r.revision<=$2) ORDER BY c.id`, w.cartID, w.revision)
	if err != nil {
		return err
	}
	tokens := []string{}
	for rows.Next() {
		var token string
		if err = rows.Scan(&token); err != nil {
			break
		}
		tokens = append(tokens, token)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, token := range tokens {
		cart, err := s.repo.GetCartByToken(ctx, token)
		if err != nil {
			return err
		}
		if cart.Status == "cancelled" || cart.Status == "expired" {
			continue
		}
		if cart.PaymentStatus == "paid" || cart.PaymentStatus == "refunded" || cart.PaymentReviewRequired {
			return errMerchantEditPaymentReview
		}
		if err := s.invalidatePendingPix(ctx, cart); err != nil {
			return fmt.Errorf("invalidating previous PIX: %w", err)
		}
		if s.couponLifecycle != nil {
			if err := s.couponLifecycle.OnCartMutated(ctx, cart.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) syncMerchantEdit(ctx context.Context, w merchantEditWork) error {
	if err := validateCartEditOrigins(ctx, s.pool, w); err != nil {
		return err
	}
	cart, err := s.repo.GetCartByToken(ctx, w.token)
	if err != nil {
		return err
	}
	ctx = logger.WithLiveEvent(logger.WithStore(ctx, cart.StoreID, cart.StoreSlug), cart.EventID)
	execution, err := s.cartEditExecution(ctx, w)
	if err != nil {
		return err
	}
	w.purchase, err = cartEditPurchaseSnapshot(ctx, s.pool, w.cartID)
	if err != nil {
		return err
	}
	if cart.Status == "cancelled" || cart.Status == "expired" {
		var state string
		if err := s.pool.QueryRow(ctx, `SELECT erp_order_state FROM carts WHERE id=$1`, w.cartID).Scan(&state); err != nil {
			return err
		}
		if execution.Remote && state != "cancelled" && state != "none" {
			return fmt.Errorf("aguardando confirmação do cancelamento no ERP")
		}
		if execution.Remote {
			if s.merchantEditERP == nil {
				return fmt.Errorf("ERP cancellation verification unavailable")
			}
			ctx = erp.WithExpectedIntegration(ctx, execution.IntegrationID)
			if err := s.merchantEditERP.VerifyERPOrderCancelled(ctx, w.cartID, w.storeID); err != nil {
				return err
			}
		}
	} else {
		if providers.ERPOrderStatus(cart.ERPOrderStatus).FechadoParaNovosItens() {
			return fmt.Errorf("pedido em situação %q: %w", cart.ERPOrderStatus, erp.ErrPedidoFaturado)
		}
		var blocked bool
		if err := s.pool.QueryRow(ctx, `SELECT payment_review_required OR COALESCE(payment_status,'') IN ('paid','refunded') FROM carts WHERE id=$1`, w.cartID).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return errMerchantEditPaymentReview
		}
		// Both the edited origin and the canonical checkout can hold an old
		// quote. Their invalidation is durable under the same pending revision.
		if err := s.refreshCartEditQuotes(ctx, w); err != nil {
			return err
		}

		owned, err := cartedit.PendingProducts(ctx, s.pool, w.cartID)
		if err != nil {
			return err
		}
		ctx = erp.WithEditedProducts(ctx, owned)
		if execution.Remote {
			ctx = erp.WithExpectedIntegration(ctx, execution.IntegrationID)
			if execution.IntegrationID != "" {
				var active bool
				if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM integrations WHERE id=$1 AND store_id=$2 AND provider=$3 AND status='active')`, execution.IntegrationID, w.storeID, execution.Provider).Scan(&active); err != nil {
					return err
				}
				if !active {
					return fmt.Errorf("the ERP integration that accepted the edit is unavailable")
				}
			}
			var state string
			var hasGrid bool
			if err := s.pool.QueryRow(ctx, `SELECT erp_order_state,EXISTS(SELECT 1 FROM cart_items ci JOIN carts member ON member.id=ci.cart_id
 JOIN products p ON p.id=ci.product_id WHERE COALESCE(member.joined_to_cart_id,member.id)=$1
 AND member.status NOT IN ('cancelled','expired') AND ci.quantity>ci.waitlisted_quantity AND COALESCE(p.external_id,'')<>'') FROM carts WHERE id=$1`, w.cartID).Scan(&state, &hasGrid); err != nil {
				return err
			}
			if s.merchantEditERP == nil {
				return fmt.Errorf("ERP reservation service unavailable")
			}
			if state == "none" || state == "converting" {
				if hasGrid || state == "converting" {
					// The accepted obligation was remote, even if the account's reservation
					// mode changed afterward. Creation adopts existing orders by their marker.
					if err := s.merchantEditERP.EnsureERPOrderForCart(ctx, w.cartID, w.storeID); err != nil {
						return err
					}
					if err := s.merchantEditERP.MutateERPOrderItems(ctx, w.cartID, w.storeID); err != nil {
						return err
					}
				}
			} else if hasGrid || len(owned) > 0 {
				// A later batch containing only manual products has no remote
				// grid to change. Pending ERP removals still appear in owned.
				if err := s.merchantEditERP.MutateERPOrderItems(ctx, w.cartID, w.storeID); err != nil {
					return err
				}
			}

			if execution.IntegrationID != "" {
				var active bool
				if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM integrations WHERE id=$1 AND store_id=$2 AND provider=$3 AND status='active')`, execution.IntegrationID, w.storeID, execution.Provider).Scan(&active); err != nil {
					return err
				}
				if !active {
					return fmt.Errorf("ERP integration changed before acknowledgement")
				}
			}
		}
	}
	products, err := s.finishMerchantEdit(ctx, w)
	if err != nil {
		return err
	}
	// Promotion happens only after the ERP confirms the release and its local
	// credit commits. The existing waitlist recovery handles interrupted work.
	for _, p := range products {
		if s.merchantEditERP != nil {
			s.merchantEditERP.ProcessWaitlistForProduct(ctx, cart.EventID, p, cart.StoreID)
		}
	}
	return nil
}

func (s *Service) finishMerchantEdit(ctx context.Context, w merchantEditWork) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM carts WHERE COALESCE(joined_to_cart_id,id)=$1 OR id IN (
 SELECT COALESCE(NULLIF(request->>'originCartId','')::uuid,cart_id) FROM cart_erp_edit_requests WHERE cart_id=$1)
 ORDER BY (id=$1) DESC,id FOR UPDATE`, w.cartID); err != nil {
		return nil, err
	}
	if err := validateCartEditOrigins(ctx, tx, w); err != nil {
		return nil, err
	}
	current, err := cartEditPurchaseSnapshot(ctx, tx, w.cartID)
	if err != nil {
		return nil, err
	}
	if w.purchase != nil && !bytes.Equal(current, w.purchase) {
		return nil, fmt.Errorf("purchase changed during ERP synchronization")
	}
	var eventID string
	if err := tx.QueryRow(ctx, `SELECT event_id::text FROM carts WHERE id=$1 FOR UPDATE`, w.cartID).Scan(&eventID); err != nil {
		return nil, err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_owner=$2::uuid AND revision=$3 AND synced_revision<$3,false) FROM cart_erp_edits WHERE cart_id=$1 FOR UPDATE`,
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
	q := s.repo.q.WithTx(tx)
	for _, r := range releases {
		if r.qty < 0 {
			return nil, fmt.Errorf("negative retained stock for product %s", r.id)
		}
		var productID pgtype.UUID
		if err := productID.Scan(r.id); err != nil {
			return nil, err
		}
		// Share cancellation's stock credit guard. A previously blocked edit
		// invalidates its mirror and waits for fresh ERP stock before release.
		if _, err := q.IncrementProductStock(ctx, sqlc.IncrementProductStockParams{
			ID: productID, Stock: pgtype.Int4{Int32: int32(r.qty), Valid: true},
		}); err != nil {
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
	if _, err := tx.Exec(ctx, `UPDATE cart_items ci SET erp_confirmed_quantity=ci.quantity-ci.waitlisted_quantity,erp_pending_since=NULL
 FROM carts c WHERE ci.cart_id=c.id AND COALESCE(c.joined_to_cart_id,c.id)=$1 AND EXISTS(
 SELECT 1 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
 WHERE r.cart_id=$1 AND r.product_id=ci.product_id AND r.revision>w.synced_revision)
	 AND (NOT EXISTS(SELECT 1 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
 WHERE r.cart_id=$1 AND r.revision>w.synced_revision AND COALESCE((r.request->'_execution'->>'remote')::boolean,true))
 OR EXISTS(SELECT 1 FROM products p WHERE p.id=ci.product_id AND COALESCE(p.external_id,'')=''))`, w.cartID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE cart_erp_edits SET synced_revision=$3,lease_owner=NULL,lease_until=NULL,
        last_error=NULL,attempts=0,blocked_at=NULL WHERE cart_id=$1 AND lease_owner=$2`, w.cartID, w.owner, w.revision); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return products, nil
}
