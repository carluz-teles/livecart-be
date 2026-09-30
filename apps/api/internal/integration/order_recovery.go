package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/erp"
)

// The checkout worker and operation sweep claim the SAME journal lease. Merely
// checking for an active worker would allow it to start during the ERP call.
func (r *Repository) ClaimERPOrderRecovery(ctx context.Context, cartID string, olderThan time.Duration) (*erp.OrderRecoveryClaim, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	claim := &erp.OrderRecoveryClaim{Release: func(error) {}}
	op := &claim.Operation
	err = tx.QueryRow(ctx, `SELECT c.id::text,COALESCE(c.store_id,e.store_id)::text,c.erp_order_state,
 COALESCE(c.external_order_id,''),COALESCE(c.erp_op_resting_state,CASE WHEN c.payment_status='paid' THEN 'confirmed' ELSE 'open' END)
 FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1 AND c.joined_to_cart_id IS NULL
 AND c.erp_order_state IN ('converting','mutating','reflecting')
 AND c.erp_op_started_at<now()-make_interval(secs=>$2) FOR UPDATE OF c SKIP LOCKED`, cartID, olderThan.Seconds()).
		Scan(&op.CartID, &op.StoreID, &op.State, &op.ExternalOrderID, &op.RestingState)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var revision int64
	var busy bool
	err = tx.QueryRow(ctx, `SELECT revision,COALESCE(lease_until>now(),false) OR blocked_at IS NOT NULL
 FROM cart_erp_edits WHERE cart_id=$1 AND revision>synced_revision FOR UPDATE`, cartID).Scan(&revision, &busy)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if busy {
			return nil, nil
		}
		claim.Pending = true
		claim.Execution, err = cartedit.ReadExecution(ctx, tx, cartID, revision)
		if err != nil {
			return nil, err
		}
		var moved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_erp_edit_requests r
 LEFT JOIN carts c ON c.id=COALESCE(NULLIF(r.request->>'originCartId','')::uuid,r.cart_id)
 JOIN cart_erp_edits w ON w.cart_id=r.cart_id WHERE r.cart_id=$1 AND r.revision>w.synced_revision
 AND (c.id IS NULL OR COALESCE(c.joined_to_cart_id,c.id)<>r.cart_id))`, cartID).Scan(&moved); err != nil {
			return nil, err
		}
		if moved {
			return nil, fmt.Errorf("edit origin moved before recovery: %w", cartedit.ErrReconciliation)
		}
		owner := uuid.NewString()
		if _, err := tx.Exec(ctx, `UPDATE cart_erp_edits SET lease_owner=$2,lease_until=now()+interval '3 minutes' WHERE cart_id=$1`, cartID, owner); err != nil {
			return nil, err
		}
		claim.Release = func(result error) {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var message any
			if result != nil {
				message = result.Error()
			}
			_, err := r.pool.Exec(cleanup, `UPDATE cart_erp_edits SET lease_owner=NULL,lease_until=NULL,
 next_attempt_at=now(),last_error=$4 WHERE cart_id=$1 AND lease_owner=$2 AND revision=$3`, cartID, owner, revision, message)
			if err != nil {
				zap.L().Error("releasing ERP recovery lease", zap.String("cart_id", cartID), zap.Error(err))
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claim, nil
}
