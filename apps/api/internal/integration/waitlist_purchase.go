package integration

import (
	"context"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/lib/httpx"
)

// New checkout units cannot consume a restock already owed to older requests.
// The shared product advisory lock also serializes comment admission/promotion.
func (r *Repository) DecrementProductStockForPurchase(ctx context.Context, productID string, quantity int) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "waitlist_product:"+productID); err != nil {
		return err
	}
	var stock int
	if err = tx.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1 AND active FOR UPDATE`, productID).Scan(&stock); err != nil {
		return err
	}
	var waiting bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM waitlist_items wi JOIN carts c ON c.id=wi.cart_id LEFT JOIN carts host ON host.id=c.joined_to_cart_id
        WHERE wi.product_id=$1 AND wi.status='waiting' AND wi.quantity>0
          AND c.status IN ('active','checkout')
          AND COALESCE(c.payment_status,'pending') NOT IN ('paid','refunded')
          AND (c.never_expires OR c.expires_at IS NULL OR c.expires_at>now())
 AND (host.id IS NULL OR (host.status IN ('active','checkout') AND host.payment_status IS DISTINCT FROM 'paid' AND host.payment_status IS DISTINCT FROM 'refunded' AND (host.never_expires OR host.expires_at IS NULL OR host.expires_at>now()))))`, productID).Scan(&waiting)
	if err != nil {
		return err
	}
	if waiting {
		return httpx.DomainError(409, httpx.CodeStockInsufficient, "a reposição está sendo destinada aos clientes na fila; aguarde a atualização do estoque")
	}
	if quantity <= 0 || stock < quantity {
		return pgx.ErrNoRows
	}
	if _, err = tx.Exec(ctx, `UPDATE products SET stock=stock-$2,erp_seq=erp_seq+1,updated_at=now() WHERE id=$1`, productID, quantity); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
