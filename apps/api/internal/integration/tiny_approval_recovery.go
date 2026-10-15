package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/ratelimit"
)

type tinyApprovalCheck struct {
	TinyApprovalCommand
	CartID  string
	Version string
}
type tinyApprovalCheckKey struct{}

func (r *Repository) deferTinyApproval(ctx context.Context, cartID string, command TinyApprovalCommand) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO tiny_approval_reconciliations(cart_id,integration_id,external_order_id)
 SELECT c.id,i.id,c.external_order_id FROM carts c JOIN integrations i ON i.store_id=c.store_id
 WHERE c.id=$1 AND c.store_id=$2 AND c.external_order_id=$3 AND i.id=$4
 AND i.provider='tiny' AND i.type='erp' AND i.status='active'
 ON CONFLICT(cart_id,integration_id,external_order_id) DO UPDATE
 SET resolved_at=NULL,reason='approval_items_pending',detected_at=now(),checked_at=now(),next_check_at=now()+interval '5 minutes'
 WHERE tiny_approval_reconciliations.resolved_at IS NOT NULL`, cartID, command.StoreID, command.OrderID, command.IntegrationID)
	if err != nil {
		return fmt.Errorf("persisting deferred Tiny approval: %w", err)
	}
	return nil
}
func (r *Repository) tinyApprovalDeferred(ctx context.Context, cartID string, command TinyApprovalCommand) (bool, error) {
	var pending bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tiny_approval_reconciliations
 WHERE cart_id=$1 AND integration_id=$2 AND external_order_id=$3 AND resolved_at IS NULL)`, cartID, command.IntegrationID, command.OrderID).Scan(&pending)
	return pending, err
}

// RecoverTinyApprovals only resumes approvals previously blocked locally. A
// fresh provider receipt and the original binding are required; being invoiced
// alone never makes a deferred purchase paid. The periodic caller is bounded.
func (s *Service) RecoverTinyApprovals(ctx context.Context) {
	rows, err := s.repo.pool.Query(ctx, `WITH picked AS (
 SELECT r.cart_id,r.integration_id,r.external_order_id FROM tiny_approval_reconciliations r
 WHERE r.resolved_at IS NULL AND r.next_check_at<=now()
 ORDER BY r.next_check_at LIMIT 5 FOR UPDATE SKIP LOCKED
 ), claimed AS (
 UPDATE tiny_approval_reconciliations r SET next_check_at=now()+interval '1 hour',checked_at=now()
 FROM picked p WHERE r.cart_id=p.cart_id AND r.integration_id=p.integration_id AND r.external_order_id=p.external_order_id
 RETURNING r.cart_id,r.integration_id,r.external_order_id
 ) SELECT r.cart_id::text,c.store_id::text,r.integration_id::text,r.external_order_id,c.xmin::text
 FROM claimed r JOIN carts c ON c.id=r.cart_id`)
	if err != nil {
		s.logger.Error("claiming deferred Tiny approvals", zap.Error(err))
		return
	}
	checks := []tinyApprovalCheck{}
	for rows.Next() {
		check := tinyApprovalCheck{TinyApprovalCommand: TinyApprovalCommand{Kind: "order_approved"}}
		if err = rows.Scan(&check.CartID, &check.StoreID, &check.IntegrationID, &check.OrderID, &check.Version); err != nil {
			break
		}
		checks = append(checks, check)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		s.logger.Error("reading deferred Tiny approvals", zap.Error(err))
		return
	}
	for _, check := range checks {
		if ctx.Err() != nil {
			return
		}
		checkCtx, cancel := context.WithTimeout(ratelimit.WithTinyCatalogRead(ctx), 45*time.Second)
		err := s.recoverTinyApproval(checkCtx, check)
		cancel()
		if err != nil {
			s.logger.Info("Tiny approval awaits reconciliation", zap.String("cart_id", check.CartID), zap.String("store_id", check.StoreID), zap.Error(err))
		}
	}
}

func (s *Service) recoverTinyApproval(ctx context.Context, check tinyApprovalCheck) error {
	var eligible, paid bool
	err := s.repo.pool.QueryRow(ctx, `SELECT c.status IN ('active','checkout') AND NOT c.purchase_closed
 AND NOT c.payment_review_required AND c.joined_to_cart_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM carts child WHERE child.joined_to_cart_id=c.id)
 AND c.external_order_id=$3 AND EXISTS(SELECT 1 FROM integrations i WHERE i.id=$4 AND i.store_id=c.store_id
 AND i.provider='tiny' AND i.type='erp' AND i.status='active'),COALESCE(c.payment_status='paid',false)
 FROM carts c WHERE c.id=$1 AND c.store_id=$2`, check.CartID, check.StoreID, check.OrderID, check.IntegrationID).Scan(&eligible, &paid)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	} // Keep the observation for individual reconciliation.
	if !paid {
		state, err := cartedit.Read(ctx, s.repo.pool, check.CartID)
		if err != nil {
			return err
		}
		if state.Pending {
			return nil
		}
		if _, err := s.repo.pool.Exec(ctx, `UPDATE tiny_approval_reconciliations SET reason='approval_payment_unverified'
   WHERE cart_id=$1 AND integration_id=$2 AND external_order_id=$3 AND resolved_at IS NULL`, check.CartID, check.IntegrationID, check.OrderID); err != nil {
			return err
		}
		integration, err := s.repo.GetActiveERP(ctx, check.StoreID)
		if err != nil {
			return err
		}
		if integration.ID != check.IntegrationID {
			return nil
		}
		provider, err := s.erpProviderFor(ctx, integration)
		if err != nil {
			return err
		}
		reader, ok := provider.(interface {
			GetOrderTotal(context.Context, string) (int64, bool, error)
		})
		if !ok {
			return nil
		}
		verifier, ok := provider.(providers.ERPRecordedPaymentVerifier)
		if !ok {
			return nil
		}
		total, _, err := reader.GetOrderTotal(ctx, check.OrderID)
		if err != nil {
			return err
		}
		if total <= 0 {
			return fmt.Errorf("deferred approval has no positive total")
		}
		if err := verifier.VerifyRecordedOrderPayment(ctx, check.OrderID, total); err != nil {
			return fmt.Errorf("verifying deferred approval receipt: %w", err)
		}
		// Guarded again under the payment transaction's cart lock. A changed cart,
		// provider binding or item set invalidates this remote observation.
		guarded := context.WithValue(ctx, tinyApprovalCheckKey{}, check)
		if _, err := s.MarkCartPaidFromERP(guarded, check.CartID, check.StoreID, total); err != nil {
			return err
		}
	}
	_, err = s.repo.pool.Exec(ctx, `UPDATE tiny_approval_reconciliations r SET resolved_at=now(),checked_at=now()
 FROM carts c WHERE c.id=r.cart_id AND c.id=$1 AND c.store_id=$2
 AND c.external_order_id=r.external_order_id AND r.external_order_id=$3 AND r.integration_id=$4
 AND c.payment_status='paid' AND NOT c.payment_review_required
 AND EXISTS(SELECT 1 FROM integrations i WHERE i.id=r.integration_id AND i.store_id=c.store_id AND i.status='active')`, check.CartID, check.StoreID, check.OrderID, check.IntegrationID)
	return err
}

func assertTinyApprovalObservation(ctx context.Context, tx pgx.Tx, cartID string) error {
	check, ok := ctx.Value(tinyApprovalCheckKey{}).(tinyApprovalCheck)
	if !ok {
		return nil
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT c.id::text FROM carts c JOIN integrations i ON i.store_id=c.store_id
 WHERE c.id=$1 AND c.store_id=$2 AND c.external_order_id=$3 AND c.xmin::text=$4
 AND c.status IN ('active','checkout') AND NOT c.purchase_closed AND NOT c.payment_review_required
 AND c.joined_to_cart_id IS NULL AND NOT EXISTS(SELECT 1 FROM carts child WHERE child.joined_to_cart_id=c.id)
 AND i.id=$5 AND i.provider='tiny' AND i.type='erp' AND i.status='active'
 AND NOT EXISTS(SELECT 1 FROM cart_items ci WHERE ci.cart_id=c.id AND ci.erp_pending_since IS NOT NULL)
 FOR SHARE OF i`, cartID, check.StoreID, check.OrderID, check.Version, check.IntegrationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.DomainError(409, httpx.CodeERPObservationChanged, "O pedido mudou durante a conferência do pagamento. A conciliação será verificada novamente.")
	}
	return err
}
