//go:build integration

package integration

import (
	"context"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

type expiryReflectionERP struct {
	*scriptedERP
	onRead func()
}

func (f *expiryReflectionERP) GetOrderItems(ctx context.Context, id string) ([]providers.ERPOrderItem, error) {
	items, err := f.scriptedERP.GetOrderItems(ctx, id)
	if f.onRead != nil {
		f.onRead()
	}
	return items, err
}

func TestReflectionSkipsExpiredAndCancelledWithoutERPReads(t *testing.T) {
	for _, status := range []string{"expired", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "aberto")
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status=$2 WHERE id=$1`, fx.cartID, status); err != nil {
				t.Fatal(err)
			}
			fake := newScriptedERP()
			report, err := newFinalisationService(fake).SyncCartFromERPOrder(t.Context(), fx.cartID, fx.storeID)
			if err != nil || report == nil || report.Skipped == "" || len(fake.callsWithPrefix("")) != 0 {
				t.Fatalf("terminal reflection should skip semantically: report=%+v err=%v calls=%v", report, err, fake.calls)
			}
		})
	}
}

func TestReflectionExpiryDuringGETSkipsWithoutChangingItems(t *testing.T) {
	fx := seedExpiredCancellation(t, "aberto")
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='checkout' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	fake := &expiryReflectionERP{scriptedERP: newScriptedERP()}
	fake.onRead = func() {
		if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status='expired' WHERE id=$1`, fx.cartID); err != nil {
			t.Fatal(err)
		}
	}
	report, err := newFinalisationService(fake).SyncCartFromERPOrder(t.Context(), fx.cartID, fx.storeID)
	if err != nil || report == nil || report.Skipped == "" {
		t.Fatalf("expiry during GET should not become ErrCartBusy: %+v %v", report, err)
	}
	var quantity int
	if err := testPool.QueryRow(t.Context(), `SELECT quantity FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&quantity); err != nil {
		t.Fatal(err)
	}
	if quantity != 1 || fake.count("GetItens:") != 1 {
		t.Fatalf("expired purchase was reflected: quantity=%d calls=%v", quantity, fake.calls)
	}
}

func TestReflectionTerminalGuardAtTransactionAndPaidReflection(t *testing.T) {
	for _, status := range []string{"expired", "cancelled", "paid"} {
		t.Run(status, func(t *testing.T) {
			fx := seedExpiredCancellation(t, "aberto")
			cartStatus, payment := status, "pending"
			if status == "paid" {
				cartStatus, payment = "checkout", "paid"
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET status=$2,payment_status=$3,erp_order_state='reflecting' WHERE id=$1`, fx.cartID, cartStatus, payment); err != nil {
				t.Fatal(err)
			}
			var external string
			if err := testPool.QueryRow(t.Context(), `SELECT external_id FROM products WHERE id=$1`, fx.productID).Scan(&external); err != nil {
				t.Fatal(err)
			}
			grid := []providers.ERPOrderItem{{ProductID: external, Quantity: 2, UnitPrice: 1000}}
			report, err := testRepo.reflectCartPurchase(t.Context(), fx.cartID, fx.storeID, fx.cartID, grid, map[string]string{external: fx.productID})
			if err != nil || report == nil {
				t.Fatalf("reflection: %+v %v", report, err)
			}
			var quantity int
			if err := testPool.QueryRow(t.Context(), `SELECT quantity FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&quantity); err != nil {
				t.Fatal(err)
			}
			want := 1
			if status == "paid" {
				want = 2
			}
			if quantity != want || (report.Skipped == "") != (status == "paid") {
				t.Fatalf("paid/terminal reflection contract broken: quantity=%d report=%+v", quantity, report)
			}
		})
	}
}
