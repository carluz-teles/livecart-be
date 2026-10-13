package integration

import (
	"context"
	"fmt"

	"livecart/apps/api/internal/integration/providers"
)

func (r *Repository) checkERPItemAccountsBlock(ctx context.Context, cartID, storeID string) (string, string, error) {
	var blocked bool
	var orderID, integrationID string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(c.external_order_id,''),COALESCE(i.id::text,''),
        EXISTS(SELECT 1 FROM active_cart_erp_item_blocks b WHERE b.cart_id=c.id AND b.retry_at>now())
        FROM carts c JOIN live_events e ON e.id=c.event_id
        LEFT JOIN LATERAL (SELECT id FROM integrations WHERE store_id=e.store_id
          AND type='erp' AND status='active' ORDER BY created_at,id LIMIT 1) i ON true
        WHERE c.id=(SELECT COALESCE(joined_to_cart_id,id) FROM carts WHERE id=$1)
          AND e.store_id=$2`, cartID, storeID).Scan(&orderID, &integrationID, &blocked)
	if err != nil {
		return "", "", fmt.Errorf("reading ERP item accounts block: %w", err)
	}
	if blocked {
		return orderID, integrationID, providers.ErrOrderAccountsLaunched
	}
	return orderID, integrationID, nil
}

func (r *Repository) blockERPItemAccounts(ctx context.Context, cartID, storeID, orderID, integrationID string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO cart_erp_item_blocks(cart_id,integration_id,external_order_id)
        SELECT c.id,$4,c.external_order_id FROM carts c JOIN live_events e ON e.id=c.event_id
        WHERE c.id=(SELECT COALESCE(joined_to_cart_id,id) FROM carts WHERE id=$1)
          AND e.store_id=$2 AND c.external_order_id=$3 AND c.external_order_id<>''
        ON CONFLICT(cart_id) DO UPDATE SET integration_id=EXCLUDED.integration_id,
          external_order_id=EXCLUDED.external_order_id,blocked_at=now(),retry_at=now()+interval '30 minutes'`,
		cartID, storeID, orderID, integrationID)
	if err != nil {
		return fmt.Errorf("saving ERP item accounts block: %w", err)
	}
	return nil
}
