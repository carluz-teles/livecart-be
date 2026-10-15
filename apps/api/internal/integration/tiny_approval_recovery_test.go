//go:build integration

package integration

import (
	"context"
	"testing"

	"livecart/apps/api/internal/erp"
)

func seedDeferredApproval(t *testing.T) (finFixture, TinyApprovalCommand) {
	t.Helper()
	fx := seedPaidCart(t, 1, 0)
	checkoutRecoveryExec(t, `UPDATE carts SET payment_status='pending',paid_at=NULL,paid_amount_cents=0,
 external_order_id='recorded-order',erp_order_status='faturado' WHERE id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=0 WHERE cart_id=$1`, fx.cartID)
	var integrationID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.blockERPItemAccounts(t.Context(), fx.cartID, fx.storeID, "recorded-order", integrationID); err != nil {
		t.Fatal(err)
	}
	return fx, TinyApprovalCommand{StoreID: fx.storeID, IntegrationID: integrationID, OrderID: "recorded-order", Kind: "order_approved"}
}
func approvalDue(t *testing.T, cartID string) {
	t.Helper()
	checkoutRecoveryExec(t, `UPDATE tiny_approval_reconciliations SET next_check_at=now() WHERE cart_id=$1`, cartID)
}
func releaseApprovalItems(t *testing.T, cartID string) {
	t.Helper()
	checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=NULL,erp_confirmed_quantity=quantity WHERE cart_id=$1`, cartID)
}
func assertApprovalUnpaid(t *testing.T, cartID string) {
	t.Helper()
	var paid string
	var ledger int
	if err := testPool.QueryRow(t.Context(), `SELECT payment_status,(SELECT count(*) FROM cart_payments p WHERE p.cart_id=c.id) FROM carts c WHERE id=$1`, cartID).Scan(&paid, &ledger); err != nil {
		t.Fatal(err)
	}
	if paid == "paid" || ledger != 0 {
		t.Fatalf("unexpected payment=%s ledger=%d", paid, ledger)
	}
}
func TestTinyApprovalBlockedIsDurableAndRequiresReceipt(t *testing.T) {
	requireDB(t)
	fx, command := seedDeferredApproval(t)
	provider := &recordedReceiptERP{scriptedERP: newScriptedERP(), unsettled: true}
	svc := newFinalisationService(provider)
	for range 2 {
		if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID)
	if err != nil || review == nil || review.Reason != "approval_items_pending" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
	approvalDue(t, fx.cartID)
	svc.RecoverTinyApprovals(t.Context())
	assertApprovalUnpaid(t, fx.cartID)
	if provider.checks != 0 {
		t.Fatal("queried provider while items blocked")
	}
	releaseApprovalItems(t, fx.cartID)
	// A redelivered webhook must use the durable review, not bypass verification.
	if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	assertApprovalUnpaid(t, fx.cartID)
	approvalDue(t, fx.cartID)
	svc.RecoverTinyApprovals(t.Context())
	assertApprovalUnpaid(t, fx.cartID)
	review, err = erp.ReadPaymentReview(t.Context(), testPool, fx.cartID)
	if err != nil || review == nil || review.Reason != "approval_payment_unverified" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
	provider.unsettled = false
	approvalDue(t, fx.cartID)
	svc.RecoverTinyApprovals(t.Context())
	approvalDue(t, fx.cartID)
	svc.RecoverTinyApprovals(t.Context())
	var ledger int
	var paid string
	if err := testPool.QueryRow(t.Context(), `SELECT payment_status,(SELECT count(*) FROM cart_payments p WHERE p.cart_id=c.id) FROM carts c WHERE id=$1`, fx.cartID).Scan(&paid, &ledger); err != nil {
		t.Fatal(err)
	}
	if paid != "paid" || ledger != 1 {
		t.Fatalf("payment=%s ledger=%d", paid, ledger)
	}
	review, err = erp.ReadPaymentReview(t.Context(), testPool, fx.cartID)
	if err != nil || review != nil {
		t.Fatalf("review not retired: %+v %v", review, err)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("wrote to ERP: %v", provider.calls)
	}
}

type approvalChangedReceipt struct {
	*recordedReceiptERP
	change func()
}

func (p *approvalChangedReceipt) VerifyRecordedOrderPayment(ctx context.Context, id string, total int64) error {
	if err := p.recordedReceiptERP.VerifyRecordedOrderPayment(ctx, id, total); err != nil {
		return err
	}
	p.change()
	return nil
}
func TestTinyApprovalRecoveryRejectsConcurrentChange(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct{ name, sql string }{
		{"binding", `UPDATE carts SET external_order_id='another-order' WHERE id=$1`},
		{"cancel", `UPDATE carts SET status='cancelled' WHERE id=$1`},
		{"cart version", `UPDATE carts SET erp_order_status=erp_order_status WHERE id=$1`},
		{"new pending item", `UPDATE cart_items SET erp_pending_since=now() WHERE cart_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, command := seedDeferredApproval(t)
			provider := &approvalChangedReceipt{recordedReceiptERP: &recordedReceiptERP{scriptedERP: newScriptedERP()}, change: func() { checkoutRecoveryExec(t, tc.sql, fx.cartID) }}
			svc := newFinalisationService(provider)
			if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			releaseApprovalItems(t, fx.cartID)
			approvalDue(t, fx.cartID)
			svc.RecoverTinyApprovals(t.Context())
			assertApprovalUnpaid(t, fx.cartID)
		})
	}
}
func TestTinyApprovalCancelledAndReplacedIntegrationStayUnpaid(t *testing.T) {
	requireDB(t)
	for _, mode := range []string{"cancelled", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			fx, command := seedDeferredApproval(t)
			provider := &recordedReceiptERP{scriptedERP: newScriptedERP()}
			svc := newFinalisationService(provider)
			if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			releaseApprovalItems(t, fx.cartID)
			if mode == "cancelled" {
				checkoutRecoveryExec(t, `UPDATE carts SET status='cancelled' WHERE id=$1`, fx.cartID)
			} else {
				checkoutRecoveryExec(t, `UPDATE integrations SET status='disconnected' WHERE id=$1`, command.IntegrationID)
			}
			approvalDue(t, fx.cartID)
			svc.RecoverTinyApprovals(t.Context())
			assertApprovalUnpaid(t, fx.cartID)
			if provider.checks != 0 {
				t.Fatal("read replaced or terminal order")
			}
		})
	}
}
