//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
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

type recordedReceiptERP struct {
	*scriptedERP
	unsettled bool
	checks    int
}

func (p *recordedReceiptERP) Name() providers.ProviderName { return providers.ProviderTiny }

func (p *recordedReceiptERP) GetOrderTotal(context.Context, string) (int64, bool, error) {
	return 1000, false, nil
}

func (p *recordedReceiptERP) SetOrderInstallments(context.Context, string, []providers.ERPInstallment) error {
	p.record("SetOrderInstallments")
	return errors.New("external installments must not be rewritten")
}

func (p *recordedReceiptERP) VerifyRecordedOrderPayment(_ context.Context, orderID string, amount int64) error {
	p.checks++
	if orderID != "recorded-order" || amount != 1000 || p.unsettled {
		return errors.New("unverified external receipt")
	}
	return nil
}

func TestExternalReceiptReconciliationResolvesOnlyReviewAndPreservesLedger(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='recorded-order',
		erp_order_state='confirmed',paid_amount_cents=1000 WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_payments
		(cart_id,amount_cents,gross_covered_cents,method,checkout_id)
		VALUES($1,1000,1000,'erp_manual','erp-recorded-order')`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	provider := &recordedReceiptERP{scriptedERP: newScriptedERP(), unsettled: true}
	svc := newFinalisationService(provider)
	if _, err := svc.RecomporParcelasDoPedidoPago(t.Context(), fx.cartID, fx.storeID); err == nil {
		t.Fatal("unsettled payment was accepted")
	}
	if review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID); err != nil || review == nil {
		t.Fatalf("review not persisted: %+v %v", review, err)
	}
	var before, after []byte
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(cp)) FROM cart_payments cp
		WHERE cart_id=$1`, fx.cartID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	provider.unsettled = false
	for range 2 {
		split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), fx.cartID, fx.storeID)
		if err != nil || split == nil || !split.Verified || split.Reescrito {
			t.Fatalf("receipt not reconciled: %+v %v", split, err)
		}
	}
	if review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID); err != nil || review != nil {
		t.Fatalf("verified review remained active: %+v %v", review, err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(cp)) FROM cart_payments cp
		WHERE cart_id=$1`, fx.cartID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(provider.calls) != 0 || provider.checks != 3 {
		t.Fatalf("payment changed or ERP written: calls=%v checks=%d", provider.calls, provider.checks)
	}
}
