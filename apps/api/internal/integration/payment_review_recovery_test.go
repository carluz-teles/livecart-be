//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"livecart/apps/api/internal/erp"
)

func seedRecoverablePaymentReview(t *testing.T) finFixture {
	t.Helper()
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='recorded-order',
 erp_order_state='confirmed',paid_amount_cents=1000,erp_order_status='aprovado' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO cart_payments
 (cart_id,amount_cents,gross_covered_cents,method,checkout_id)
 VALUES($1,1000,1000,'erp_manual','erp-recorded-order')`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	paid, total := int64(1000), int64(1000)
	if err := testRepo.RecordERPFinancialReview(t.Context(), fx.cartID, fx.storeID, erp.PaymentReview{
		ExternalOrderID: "recorded-order", Reason: "installments_unverified",
		PaidCents: &paid, OrderTotalCents: &total, CheckedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM erp_payment_reviews WHERE cart_id=$1`, fx.cartID); err != nil {
			t.Error(err)
		}
	})
	return fx
}

func paymentLedgerSnapshot(t *testing.T, cartID string) string {
	t.Helper()
	var snapshot string
	if err := testPool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'cart',to_jsonb(c), 'payments',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM cart_payments p WHERE p.cart_id=c.id),
 'order',(SELECT to_jsonb(o) FROM orders o WHERE o.cart_id=c.id),
 'events',(SELECT count(*) FROM event_outbox WHERE payload->>'cart_id'=c.id::text))::text
 FROM carts c WHERE c.id=$1`, cartID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPaymentReviewRecoveryOnlyClearsVerifiedObservation(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct {
		name      string
		unsettled bool
	}{
		{name: "settled receipts retire the alert"},
		{name: "open title preserves the alert", unsettled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedRecoverablePaymentReview(t)
			provider := &recordedReceiptERP{scriptedERP: newScriptedERP(), unsettled: tc.unsettled}
			svc := newFinalisationService(provider)
			before := paymentLedgerSnapshot(t, fx.cartID)
			svc.RecoverERPFinancialReviews(t.Context())
			svc.RecoverERPFinancialReviews(t.Context())
			review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID)
			if err != nil || (review != nil) != tc.unsettled {
				t.Fatalf("review=%+v err=%v unsettled=%v", review, err, tc.unsettled)
			}
			if after := paymentLedgerSnapshot(t, fx.cartID); before != after {
				t.Fatal("verification modified money, cart, order or emitted an event")
			}
			if provider.checks != 1 || len(provider.calls) != 0 {
				t.Fatalf("unexpected provider work: reads=%d writes=%v", provider.checks, provider.calls)
			}
		})
	}
}

func TestPaymentReviewRecoveryLeavesOperationalAndHistoricalCasesAlone(t *testing.T) {
	requireDB(t)
	cases := []struct {
		name string
		sql  string
	}{
		{"gateway payment", `UPDATE cart_payments SET method='pix' WHERE cart_id=$1`},
		{"unknown payment origin", `UPDATE cart_payments SET method=NULL WHERE cart_id=$1`},
		{"different ERP payment reference", `UPDATE cart_payments SET checkout_id='erp-other' WHERE cart_id=$1`},
		{"known value discrepancy", `UPDATE erp_payment_reviews SET order_total_cents=1200 WHERE cart_id=$1`},
		{"ambiguous receipt", `UPDATE carts SET payment_review_required=true WHERE id=$1`},
		{"mutation in flight", `UPDATE carts SET erp_order_state='mutating' WHERE id=$1`},
		{"pending items", `UPDATE cart_items SET erp_pending_since=now() WHERE cart_id=$1`},
		{"expired sale", `UPDATE carts SET status='expired' WHERE id=$1`},
		{"no materialised order", `DELETE FROM orders WHERE cart_id=$1`},
		{"paid ledger differs", `UPDATE cart_payments SET amount_cents=999 WHERE cart_id=$1`},
		{"new observation is not due", `UPDATE erp_payment_reviews SET checked_at=now() WHERE cart_id=$1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedRecoverablePaymentReview(t)
			if _, err := testPool.Exec(t.Context(), tc.sql, fx.cartID); err != nil {
				t.Fatal(err)
			}
			provider := &recordedReceiptERP{scriptedERP: newScriptedERP()}
			svc := newFinalisationService(provider)
			svc.RecoverERPFinancialReviews(t.Context())
			if provider.checks != 0 || len(provider.calls) != 0 {
				t.Fatalf("ineligible sale reached ERP: %d %v", provider.checks, provider.calls)
			}
			review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID)
			if err != nil || review == nil {
				t.Fatalf("review incorrectly cleared: %+v %v", review, err)
			}
		})
	}
}

type changedReceiptERP struct {
	*recordedReceiptERP
	change func() error
}

func (p *changedReceiptERP) VerifyRecordedOrderPayment(ctx context.Context, orderID string, paid int64) error {
	if err := p.recordedReceiptERP.VerifyRecordedOrderPayment(ctx, orderID, paid); err != nil {
		return err
	}
	return p.change()
}

func TestPaymentReviewRecoveryRejectsStaleObservation(t *testing.T) {
	requireDB(t)
	cases := []struct {
		name string
		sql  string
	}{
		{"ERP binding changed", `UPDATE carts SET external_order_id='replacement' WHERE id=$1`},
		{"cart changed during the read", `UPDATE carts SET erp_order_status='faturado' WHERE id=$1`},
		{"newer financial observation", `UPDATE erp_payment_reviews SET checked_at=now() WHERE cart_id=$1`},
		{"payment origin changed", `UPDATE cart_payments SET method='pix' WHERE cart_id=$1`},
		{"integration disabled", `UPDATE integrations SET status='disconnected' WHERE store_id=(SELECT store_id FROM carts WHERE id=$1)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedRecoverablePaymentReview(t)
			provider := &changedReceiptERP{
				recordedReceiptERP: &recordedReceiptERP{scriptedERP: newScriptedERP()},
				change: func() error {
					_, err := testPool.Exec(t.Context(), tc.sql, fx.cartID)
					if err != nil {
						t.Fatal(err)
					}
					return nil
				},
			}
			svc := newFinalisationService(provider)
			svc.RecoverERPFinancialReviews(t.Context())
			if provider.checks != 1 {
				t.Fatalf("read not exercised: %d", provider.checks)
			}
			if review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID); err != nil || review == nil {
				t.Fatalf("stale observation cleared review: %+v %v", review, err)
			}
		})
	}
}

func TestPaymentReviewRecoveryClaimsAreExclusiveAndScheduled(t *testing.T) {
	requireDB(t)
	fx := seedRecoverablePaymentReview(t)
	var wg sync.WaitGroup
	results := make(chan []externalPaymentReviewCheck, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := testRepo.claimExternalPaymentReviews(t.Context())
			results <- rows
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for rows := range results {
		for _, row := range rows {
			if row.CartID == fx.cartID {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("same review claimed %d times", count)
	}
	var scheduled bool
	if err := testPool.QueryRow(t.Context(), `SELECT next_check_at>now()+interval '50 minutes'
 FROM erp_payment_reviews WHERE cart_id=$1`, fx.cartID).Scan(&scheduled); err != nil || !scheduled {
		t.Fatalf("next check not durable: %v %v", scheduled, err)
	}
}

func TestPaymentReviewRecoveryReadFailureKeepsAlert(t *testing.T) {
	requireDB(t)
	fx := seedRecoverablePaymentReview(t)
	provider := &changedReceiptERP{
		recordedReceiptERP: &recordedReceiptERP{scriptedERP: newScriptedERP()},
		change:             func() error { return errors.New("temporary read failure") },
	}
	newFinalisationService(provider).RecoverERPFinancialReviews(t.Context())
	if review, err := erp.ReadPaymentReview(t.Context(), testPool, fx.cartID); err != nil || review == nil {
		t.Fatalf("read failure cleared alert: %+v %v", review, err)
	}
}

func TestPaymentReviewRecoveryWaitsForConcurrentCartMutation(t *testing.T) {
	requireDB(t)
	fx := seedRecoverablePaymentReview(t)
	checks, err := testRepo.claimExternalPaymentReviews(t.Context())
	if err != nil || len(checks) != 1 {
		t.Fatalf("claim: %v %v", checks, err)
	}
	change, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer change.Rollback(context.Background()) //nolint:errcheck
	if _, err := change.Exec(t.Context(), `UPDATE carts SET external_order_id='replacement' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		resolved bool
		err      error
	}
	finished := make(chan outcome, 1)
	go func() {
		resolved, err := testRepo.resolveVerifiedExternalPaymentReview(ctx, checks[0], time.Now())
		finished <- outcome{resolved, err}
	}()
	// Wait for the resolver's row lock, rather than relying on scheduler timing.
	for {
		var waiting bool
		if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock'
 AND query LIKE '%c.xmin::text=$3 FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case result := <-finished:
			t.Fatalf("resolver ignored concurrent cart lock: %+v", result)
		case <-ctx.Done():
			t.Fatal("resolver did not acquire the cart lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := change.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-finished
	if result.err != nil || result.resolved {
		t.Fatalf("concurrent binding change cleared the review: %+v", result)
	}
	if review, err := erp.ReadPaymentReview(ctx, testPool, fx.cartID); err != nil || review == nil {
		t.Fatalf("concurrent change lost financial alert: %+v %v", review, err)
	}
}
