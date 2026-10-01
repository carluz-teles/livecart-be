//go:build integration

package order_test

import (
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/order"
)

func TestERPFinancialReviewAppearsInAttentionAndDetailOnlyForOwner(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1000, 0, 0)
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_payment_reviews(cart_id,external_order_id,reason,paid_cents,order_total_cents)
        VALUES($1,'erp-order','total_below_paid',1000,800)`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	attention := true
	filters := order.OrderFilters{NeedsAttention: &attention}
	if rows := listar(t, fx.storeID, filters); len(rows) != 1 {
		t.Fatalf("review not listed: %d", len(rows))
	}
	repo := order.NewRepository(testPool)
	stats, err := repo.GetStats(t.Context(), fx.storeID, "", filters)
	if err != nil || stats.TotalOrders != 1 {
		t.Fatalf("attention counts differ: %+v %v", stats, err)
	}
	svc := order.NewService(repo, zap.NewNop())
	detail, err := svc.GetDetailByID(t.Context(), fx.cartID, fx.storeID)
	if err != nil || detail.ERPPaymentReview == nil {
		t.Fatalf("missing detail review: %+v %v", detail, err)
	}
	response := order.NewOrderDetailResponse(*detail)
	if response.ERPPaymentReview == nil {
		t.Fatal("response dropped financial review")
	}
	otherStore, _ := seedIsolatedStore(t, "other-finance-review")
	if _, err := svc.GetDetailByID(t.Context(), fx.cartID, otherStore); err == nil {
		t.Fatal("another store read financial review")
	}
}
