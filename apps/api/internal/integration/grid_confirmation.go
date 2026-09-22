package integration

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"go.uber.org/zap"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
)

func (r *Repository) ConfirmERPGrid(ctx context.Context, cartID string, grid []providers.ERPOrderItem) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if err := confirmERPGridTx(ctx, tx, cartID, grid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func confirmERPGridTx(ctx context.Context, tx pgx.Tx, cartID string, grid []providers.ERPOrderItem) error {
	if err := tx.QueryRow(ctx, `SELECT COALESCE(joined_to_cart_id,id)::text FROM carts WHERE id=$1`, cartID).Scan(&cartID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM carts WHERE COALESCE(joined_to_cart_id,id)=$1 ORDER BY (id=$1) DESC,id FOR UPDATE`, cartID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT p.id FROM products p WHERE EXISTS(SELECT 1 FROM cart_items ci JOIN carts c ON c.id=ci.cart_id WHERE ci.product_id=p.id AND COALESCE(c.joined_to_cart_id,c.id)=$1) ORDER BY p.id FOR UPDATE`, cartID); err != nil {
		return err
	}
	byProduct := make(map[string]map[int64]int)
	for _, item := range grid {
		if byProduct[item.ProductID] == nil {
			byProduct[item.ProductID] = make(map[int64]int)
		}
		byProduct[item.ProductID][item.UnitPrice] += item.Quantity
	}
	for productID, prices := range byProduct {
		wanted, err := json.Marshal(prices)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `WITH members AS (
            SELECT ci.id,ci.quantity-ci.waitlisted_quantity AS qty
            FROM cart_items ci JOIN products p ON p.id=ci.product_id JOIN carts c ON c.id=ci.cart_id
            WHERE COALESCE(c.joined_to_cart_id,c.id)=$1 AND p.external_id=$2
        ), actual AS (
            SELECT l.unit_price AS price,SUM(l.quantity-l.waitlisted_quantity)::bigint AS qty
            FROM members m JOIN cart_item_price_lots l ON l.cart_item_id=m.id
            WHERE l.quantity>l.waitlisted_quantity GROUP BY l.unit_price
        ), desired AS (
            SELECT key::bigint AS price,value::bigint AS qty FROM jsonb_each_text($3::jsonb)
        ), comparison AS (
            SELECT NOT EXISTS ((SELECT * FROM actual EXCEPT SELECT * FROM desired)
                UNION ALL (SELECT * FROM desired EXCEPT SELECT * FROM actual)) AS matches,
                (SELECT COUNT(*) FROM members) AS n, (SELECT SUM(qty) FROM desired) AS qty
        ), changed AS (
            UPDATE cart_items ci SET
                erp_confirmed_quantity=CASE WHEN t.matches THEN m.qty WHEN t.n=1 THEN t.qty ELSE ci.erp_confirmed_quantity END,
                erp_pending_since=CASE WHEN t.matches THEN NULL ELSE ci.erp_pending_since END
            FROM members m CROSS JOIN comparison t WHERE ci.id=m.id
                AND (ci.erp_confirmed_quantity IS DISTINCT FROM CASE WHEN t.matches THEN m.qty WHEN t.n=1 THEN t.qty ELSE ci.erp_confirmed_quantity END
                    OR (t.matches AND ci.erp_pending_since IS NOT NULL))
            RETURNING ci.product_id
        ) UPDATE products SET erp_seq=erp_seq+1 WHERE id IN (SELECT product_id FROM changed)`,
			cartID, productID, string(wanted))
		if err != nil {
			return err
		}
	}
	return nil
}

// Only new, versioned pending lines are eligible. Legacy incident repairs need
// their separately reviewed reconciliation plan.
func (s *Service) RecoverPendingERPItems(ctx context.Context) {
	rows, err := s.repo.pool.Query(ctx, `WITH candidates AS (
        SELECT c.id FROM carts c WHERE c.status NOT IN ('cancelled','expired')
        AND NOT c.purchase_closed AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
        AND erp_order_accepts_items(c.erp_order_status)
        AND NOT EXISTS(SELECT 1 FROM carts host WHERE host.id=c.joined_to_cart_id
          AND (NOT erp_order_accepts_items(host.erp_order_status) OR host.purchase_closed
            OR host.payment_status IN ('paid','refunded') OR host.status IN ('cancelled','expired')))
        AND NOT EXISTS (SELECT 1 FROM cart_erp_edits w WHERE w.cart_id=c.id AND w.revision>w.synced_revision)
        AND (c.erp_items_retry_at IS NULL OR c.erp_items_retry_at<now())
        AND EXISTS (SELECT 1 FROM cart_items ci WHERE ci.cart_id=c.id
            AND ci.erp_pending_since<now()-interval '30 seconds' AND ci.erp_confirmed_quantity IS NOT NULL)
        ORDER BY c.erp_items_retry_at NULLS FIRST,c.id LIMIT 10 FOR UPDATE SKIP LOCKED
    ), claimed AS (
        UPDATE carts c SET erp_items_retry_at=now()+interval '3 minutes'
        FROM candidates p WHERE c.id=p.id RETURNING c.id,c.event_id
    ) SELECT c.id::text,e.store_id::text,c.event_id::text FROM claimed c JOIN live_events e ON e.id=c.event_id`)
	if err != nil {
		s.logger.Error("listing pending ERP items", zap.Error(err))
		return
	}
	type pendingCart struct{ cart, store, event string }
	var carts []pendingCart
	for rows.Next() {
		var c pendingCart
		if err := rows.Scan(&c.cart, &c.store, &c.event); err != nil {
			rows.Close()
			return
		}
		carts = append(carts, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.logger.Error("reading pending ERP items", zap.Error(err))
		return
	}
	started := time.Now()
	attempted, acknowledged, deferred := 0, 0, 0
	defer func() {
		if len(carts) > 0 {
			s.logger.Info("ERP item recovery batch", zap.Int("selected", len(carts)),
				zap.Int("attempted", attempted), zap.Int("acknowledged", acknowledged),
				zap.Int("deferred", deferred), zap.Duration("duration", time.Since(started)))
		}
	}()
	for _, c := range carts {
		if ctx.Err() != nil {
			return
		}
		attempted++
		if err := s.ReserveStockInERP(ctx, c.store, c.cart, c.event, "", 0, 0, ""); err != nil {
			deferred++
			s.logger.Warn("ERP cart remains pending", zap.String("cart_id", c.cart), zap.String("store_id", c.store), zap.Error(err))
			continue
		}
		// Local reservation mode intentionally has no ERP order. Apply the same
		// guarded acknowledgement used by the immediate comment path.
		items, err := s.repo.ListNonWaitlistedCartItems(ctx, c.cart)
		if err != nil {
			deferred++
			s.logger.Warn("reading recovered cart", zap.Error(err))
			continue
		}
		ackFailed := false
		for _, item := range items {
			if err := s.repo.ConfirmarItemNoERP(ctx, c.cart, item.ProductID); err != nil {
				ackFailed = true
				s.logger.Warn("acknowledging recovered cart item", zap.Error(err))
			}
		}
		if ackFailed {
			deferred++
			continue
		}
		var stillPending bool
		if err := s.repo.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cart_items WHERE cart_id=$1 AND erp_pending_since IS NOT NULL)`, c.cart).Scan(&stillPending); err != nil {
			deferred++
			s.logger.Warn("checking recovered grid acknowledgement", zap.String("cart_id", c.cart), zap.Error(err))
			continue
		}
		if stillPending {
			deferred++
			s.logger.Info("ERP item recovery awaits verified grid", zap.String("cart_id", c.cart))
			continue
		}
		acknowledged++
		s.logger.Info("ERP item recovery acknowledged", zap.String("cart_id", c.cart),
			zap.String("store_id", c.store), zap.String("event_id", c.event), zap.Int("items", len(items)))
	}
}

func (r *Repository) PendingERPGridProducts(ctx context.Context, cartID string) ([]string, error) {
	return cartedit.PendingProducts(ctx, r.pool, cartID)
}
