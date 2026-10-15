package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

// These checks only retire stale observations for an existing materialised sale.
// Gateway payments, mixed ledgers, grouped purchases and value discrepancies
// require their own reconciliation; they must never enter an installment writer.
const externalPaymentReviewEligible = `
 r.resolved_at IS NULL AND r.reason='installments_unverified'
 AND c.external_order_id=r.external_order_id
 AND c.erp_order_state='confirmed' AND c.payment_status='paid'
 AND c.status IN ('active','checkout') AND c.paid_amount_cents>0
 AND NOT c.payment_review_required AND NOT c.purchase_closed
 AND c.joined_to_cart_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM carts child WHERE child.joined_to_cart_id=c.id)
 AND EXISTS(SELECT 1 FROM orders o WHERE o.cart_id=c.id AND o.store_id=c.store_id)
 AND (r.paid_cents IS NULL OR r.paid_cents=c.paid_amount_cents)
 AND (r.order_total_cents IS NULL OR r.order_total_cents=c.paid_amount_cents)
 AND EXISTS(SELECT 1 FROM cart_payments p WHERE p.cart_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM cart_payments p WHERE p.cart_id=c.id
     AND (p.method IS DISTINCT FROM 'erp_manual' OR p.checkout_id<>'erp-'||c.external_order_id OR p.amount_cents<=0))
 AND c.paid_amount_cents=(SELECT sum(p.amount_cents) FROM cart_payments p WHERE p.cart_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM cart_erp_edits w WHERE w.cart_id=c.id AND w.revision>w.synced_revision)
 AND NOT EXISTS(SELECT 1 FROM cart_items ci WHERE ci.cart_id=c.id AND ci.erp_pending_since IS NOT NULL)`

type externalPaymentReviewCheck struct {
	CartID          string
	StoreID         string
	IntegrationID   string
	ExternalOrderID string
	PaidCents       int64
	CheckedAt       time.Time
	CartVersion     string
}

// One sale per store per batch prevents a busy merchant from consuming all of
// the batch budget. SKIP LOCKED and next_check_at also coordinate API replicas.
func (r *Repository) claimExternalPaymentReviews(ctx context.Context) ([]externalPaymentReviewCheck, error) {
	rows, err := r.pool.Query(ctx, `WITH candidates AS MATERIALIZED (
 SELECT r.cart_id,c.store_id,i.id AS integration_id,r.next_check_at,
   row_number() OVER(PARTITION BY c.store_id ORDER BY r.next_check_at,r.cart_id) AS position
 FROM erp_payment_reviews r JOIN carts c ON c.id=r.cart_id
 JOIN integrations i ON i.store_id=c.store_id AND i.type='erp' AND i.provider='tiny' AND i.status='active'
 WHERE `+externalPaymentReviewEligible+`
   AND r.next_check_at<=now() AND r.checked_at<now()-interval '15 minutes'
 ), picked AS (
 SELECT r.cart_id,k.integration_id FROM erp_payment_reviews r JOIN candidates k ON k.cart_id=r.cart_id
 WHERE k.position=1 AND r.resolved_at IS NULL AND r.next_check_at<=now()
 ORDER BY k.next_check_at,r.cart_id LIMIT 5 FOR UPDATE OF r SKIP LOCKED
 ), claimed AS (
 UPDATE erp_payment_reviews r SET next_check_at=now()+interval '1 hour'
 FROM picked p WHERE r.cart_id=p.cart_id
 RETURNING r.cart_id,r.external_order_id,r.checked_at,p.integration_id
 ) SELECT q.cart_id::text,c.store_id::text,q.integration_id::text,q.external_order_id,
   c.paid_amount_cents,q.checked_at,c.xmin::text FROM claimed q JOIN carts c ON c.id=q.cart_id`)
	if err != nil {
		return nil, fmt.Errorf("claiming external payment observations: %w", err)
	}
	defer rows.Close()
	checks := []externalPaymentReviewCheck{}
	for rows.Next() {
		var check externalPaymentReviewCheck
		if err := rows.Scan(
			&check.CartID, &check.StoreID, &check.IntegrationID, &check.ExternalOrderID,
			&check.PaidCents, &check.CheckedAt, &check.CartVersion,
		); err != nil {
			return nil, fmt.Errorf("reading external payment observation: %w", err)
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading external payment observations: %w", err)
	}
	return checks, nil
}

func (r *Repository) resolveVerifiedExternalPaymentReview(
	ctx context.Context, check externalPaymentReviewCheck, verifiedAt time.Time,
) (bool, error) {
	// A newer webhook, payment, edit or binding invalidates this observation.
	// Lock the cart before the final check: an UPDATE of only the review would
	// otherwise read a stale cart snapshot while a concurrent payment commits.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("starting payment observation resolution: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var cartID string
	err = tx.QueryRow(ctx, `SELECT c.id::text FROM carts c
 WHERE c.id=$1 AND c.store_id=$2 AND c.xmin::text=$3 FOR UPDATE`,
		check.CartID, check.StoreID, check.CartVersion).Scan(&cartID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("locking verified payment purchase: %w", err)
	}
	result, err := tx.Exec(ctx, `UPDATE erp_payment_reviews r
 SET resolved_at=now(),checked_at=$7 FROM carts c
 WHERE c.id=r.cart_id AND r.cart_id=$1 AND c.store_id=$2
   AND r.external_order_id=$3 AND c.paid_amount_cents=$4
   AND r.checked_at=$5 AND c.xmin::text=$6
   AND EXISTS(SELECT 1 FROM integrations i WHERE i.id=$8 AND i.store_id=c.store_id
     AND i.type='erp' AND i.provider='tiny' AND i.status='active')
   AND `+externalPaymentReviewEligible,
		check.CartID, check.StoreID, check.ExternalOrderID, check.PaidCents,
		check.CheckedAt, check.CartVersion, verifiedAt, check.IntegrationID,
	)
	if err != nil {
		return false, fmt.Errorf("resolving verified external payment observation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing verified payment observation: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

// RecoverERPFinancialReviews uses only Tiny's read-only receipt verifier. It
// cannot create orders, rewrite installments, record money or emit order.paid.
func (s *Service) RecoverERPFinancialReviews(ctx context.Context) {
	s.RecoverTinyApprovals(ctx)
	checks, err := s.repo.claimExternalPaymentReviews(ctx)
	if err != nil {
		s.logger.Error("listing external payment observations for verification", zap.Error(err))
		return
	}
	for _, check := range checks {
		if ctx.Err() != nil {
			return
		}
		verifyCtx, cancel := context.WithTimeout(ratelimit.WithTinyCatalogRead(ctx), 45*time.Second)
		resolved, err := s.verifyExternalPaymentReview(verifyCtx, check)
		cancel()
		fields := []zap.Field{zap.String("cart_id", check.CartID), zap.String("store_id", check.StoreID)}
		if err != nil {
			// Pending titles and temporary read failures leave the existing review
			// visible. The durable schedule will check it again without a webhook.
			s.logger.Info("external payment observation remains unresolved", append(fields, zap.Error(err))...)
			continue
		}
		if resolved {
			s.logger.Info("external payment observation verified from Tiny receipts", fields...)
		}
	}
}

func (s *Service) verifyExternalPaymentReview(
	ctx context.Context, check externalPaymentReviewCheck,
) (bool, error) {
	integration, err := s.repo.GetActiveERP(ctx, check.StoreID)
	if err != nil {
		return false, err
	}
	if integration.ID != check.IntegrationID {
		return false, nil
	}
	provider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		return false, err
	}
	verifier, ok := provider.(providers.ERPRecordedPaymentVerifier)
	if !ok || provider.Name() != providers.ProviderTiny {
		return false, nil
	}
	startedAt := time.Now().UTC()
	if err := verifier.VerifyRecordedOrderPayment(ctx, check.ExternalOrderID, check.PaidCents); err != nil {
		return false, fmt.Errorf("verifying recorded Tiny receipt: %w", err)
	}
	return s.repo.resolveVerifiedExternalPaymentReview(ctx, check, startedAt)
}
