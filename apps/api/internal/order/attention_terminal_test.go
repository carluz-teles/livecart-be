package order_test

import (
	"livecart/apps/api/internal/order"
	"testing"
)

func TestTerminalOrdersLeaveAttentionDespiteHistoricalFailures(t *testing.T) {
	requireDB(t)
	for _, state := range []string{"cancelled", "expired", "checkout"} {
		t.Run(state, func(t *testing.T) {
			fx := seedPaidCart(t, 1, 1000, 0, 0)
			if err := newListener(t).OnCartPaid(t.Context(), fx.cartID, fx.storeID, 1000, nil); err != nil {
				t.Fatal(err)
			}
			orderID, shipmentID := seedShipment(t, fx.storeID, fx.cartID)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := testPool.Exec(t.Context(), sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE carts SET status=$2,payment_status='refunded',external_order_id='orphan-order',erp_order_status='aberto' WHERE id=$1`, fx.cartID, state)
			exec(`UPDATE cart_items SET erp_pending_since=now() WHERE cart_id=$1`, fx.cartID)
			exec(`UPDATE order_payments SET erp_finalisation_status='failed' WHERE order_id=$1`, orderID)
			exec(`UPDATE shipments SET status='issue' WHERE id=$1`, shipmentID)
			for _, attention := range []bool{true, false} {
				filters := order.OrderFilters{NeedsAttention: &attention}
				rows := listar(t, fx.storeID, filters)
				want := 0
				if attention == (state == "checkout") {
					want = 1
				}
				stats, err := order.NewRepository(testPool).GetStats(t.Context(), fx.storeID, "", filters)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != want || int(stats.TotalOrders) != want {
					t.Fatalf("attention=%t list=%d stats=%d want=%d", attention, len(rows), stats.TotalOrders, want)
				}
			}
			// A real unresolved payment still needs a merchant decision.
			exec(`UPDATE carts SET payment_review_required=true WHERE id=$1`, fx.cartID)
			attention := true
			if len(listar(t, fx.storeID, order.OrderFilters{NeedsAttention: &attention})) != 1 {
				t.Fatal("payment review was hidden")
			}
		})
	}
}
