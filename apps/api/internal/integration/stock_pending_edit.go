package integration

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var errERPStockPendingEdit = errors.New("ERP stock awaits pending order edit reconciliation")

// Preserve the invalidation before acknowledging a webhook or advancing a
// catalog scan. A blocked, idle edit can read stock with conservative holds;
// actionable edits and active workers still require reconciliation first.
func (r *Repository) deferStockForPendingEdit(ctx context.Context, productID string) error {
	var id string
	err := r.pool.QueryRow(ctx, `INSERT INTO erp_stock_sync_state(product_id,last_attempt_at,deferred_at)
		SELECT p.id,'epoch',now() FROM products p WHERE p.id=$1
		AND EXISTS(SELECT 1 FROM cart_erp_edit_requests r JOIN cart_erp_edits w ON w.cart_id=r.cart_id
			WHERE r.product_id=p.id AND r.revision>w.synced_revision
			AND (w.blocked_at IS NULL OR w.lease_until>now()))
		ON CONFLICT(product_id) DO UPDATE SET
		last_attempt_at=CASE WHEN erp_stock_sync_state.deferred_at IS NULL THEN 'epoch'::timestamptz
			ELSE erp_stock_sync_state.last_attempt_at END,
		deferred_at=COALESCE(erp_stock_sync_state.deferred_at,EXCLUDED.deferred_at)
		RETURNING product_id::text`, productID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("persisting deferred stock check: %w", err)
	}
	return fmt.Errorf("product %s: %w", id, errERPStockPendingEdit)
}
