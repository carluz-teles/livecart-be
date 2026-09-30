//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"livecart/apps/api/internal/erp"
)

func TestERPFinancialReviewIsScopedAndPreservesPaymentState(t *testing.T) {
	requireDB(t)
	c := semearCarrinho(t)
	var storeID string
	if err := testPool.QueryRow(t.Context(), `UPDATE carts SET external_order_id='review-order',payment_status='paid',paid_amount_cents=332107 WHERE id=$1 RETURNING store_id::text`, c.id).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	paid, total := int64(332107), int64(308727)
	review := erp.PaymentReview{ExternalOrderID: "review-order", Reason: "total_below_paid", PaidCents: &paid, OrderTotalCents: &total}
	if err := testRepo.RecordERPFinancialReview(t.Context(), c.id, uuid.NewString(), review); err == nil {
		t.Fatal("wrong store wrote review")
	}
	for range 2 {
		if err := testRepo.RecordERPFinancialReview(t.Context(), c.id, storeID, review); err != nil {
			t.Fatal(err)
		}
	}
	got, err := erp.ReadPaymentReview(t.Context(), testPool, c.id)
	if err != nil || got == nil || *got.PaidCents != paid || *got.OrderTotalCents != total {
		t.Fatalf("review=%+v err=%v", got, err)
	}
	var amount int64
	var paymentStatus string
	var paymentReview bool
	if err := testPool.QueryRow(t.Context(), `SELECT payment_status,paid_amount_cents,payment_review_required FROM carts WHERE id=$1`, c.id).Scan(&paymentStatus, &amount, &paymentReview); err != nil {
		t.Fatal(err)
	}
	if amount != paid || paymentStatus != "paid" || paymentReview {
		t.Fatalf("payment changed: %d %s %v", amount, paymentStatus, paymentReview)
	}
	if err := testRepo.ResolveERPFinancialReview(t.Context(), c.id, storeID, "other-order", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := erp.ReadPaymentReview(t.Context(), testPool, c.id); err != nil || got == nil {
		t.Fatal("different ERP order cleared review")
	}
	// An older verification must not clear or overwrite a more recent failure.
	stale := got.CheckedAt.Add(-time.Second)
	if err := testRepo.ResolveERPFinancialReview(t.Context(), c.id, storeID, review.ExternalOrderID, stale); err != nil {
		t.Fatal(err)
	}
	review.CheckedAt = stale
	if err := testRepo.RecordERPFinancialReview(t.Context(), c.id, storeID, review); err == nil {
		t.Fatal("older observation overwrote current review")
	}
	if current, err := erp.ReadPaymentReview(t.Context(), testPool, c.id); err != nil || current == nil || !current.CheckedAt.Equal(got.CheckedAt) {
		t.Fatalf("stale observation changed review: %+v %v", current, err)
	}
	if err := testRepo.ResolveERPFinancialReview(t.Context(), c.id, storeID, review.ExternalOrderID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := erp.ReadPaymentReview(t.Context(), testPool, c.id); err != nil || got != nil {
		t.Fatalf("resolved review still active: %+v %v", got, err)
	}
}
