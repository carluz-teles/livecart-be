//go:build integration

package integration

import (
	"context"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"testing"
)

func TestStockAcknowledgement_PendingUnitsStayUnavailable(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE carts SET erp_order_state='open',external_order_id='test-order',erp_order_status='aberto',payment_status='pending' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=1 WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var ext string
	if err := testPool.QueryRow(ctx, `SELECT external_id FROM products WHERE id=$1`, fx.productID).Scan(&ext); err != nil {
		t.Fatal(err)
	}
	svc := &Service{repo: testRepo, logger: zap.NewNop()}
	integration := &IntegrationRow{StoreID: fx.storeID, Provider: "tiny"}
	// Tiny's 9 available already discount the first unit, but not the second.
	if got := svc.PortaoAPartirDoSaldoDoERP(ctx, integration, ext, 9); got != 8 {
		t.Fatalf("available=%d, want 8", got)
	}
	seen := seqDoProduto(t, fx.productID)
	// Confirmation races the in-flight stock GET. Even if pending clears, that
	// GET still describes stock before the second unit was reserved.
	if err := testRepo.ConfirmERPGrid(ctx, fx.cartID, []providers.ERPOrderItem{{ProductID: ext, Quantity: 2, UnitPrice: 1000}}); err != nil {
		t.Fatal(err)
	}
	if applied, err := testRepo.ApplyERPStockMirror(ctx, fx.productID, 9, seen); err != nil || applied {
		t.Fatalf("stale stock applied after ACK: %v %v", applied, err)
	}
	if got := svc.PortaoAPartirDoSaldoDoERP(ctx, integration, ext, 8); got != 8 {
		t.Fatalf("acknowledged unit was discounted twice: %d", got)
	}
}
