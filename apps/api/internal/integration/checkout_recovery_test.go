package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
)

func checkoutRecoveryExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsBlockPreservesCommentAndResumesAfterCooldown(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedPaidCart(t, 2, 0)
	checkoutRecoveryExec(t, `UPDATE carts SET payment_status='pending',external_order_id='account-order',erp_order_status='aberto',erp_order_state='open' WHERE id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=1 WHERE cart_id=$1`, fx.cartID)
	orderID, integrationID, err := testRepo.checkERPItemAccountsBlock(ctx, fx.cartID, fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testRepo.blockERPItemAccounts(ctx, fx.cartID, fx.storeID, orderID, integrationID); err != nil {
		t.Fatal(err)
	}
	id := "accounts-" + fx.cartID
	owner, _, err := testRepo.BeginCommentWork(ctx, id, []byte(`{"CommentID":"accounts"}`))
	if err != nil {
		t.Fatal(err)
	}
	checkoutRecoveryExec(t, `INSERT INTO cart_item_events(platform_comment_id,product_id,cart_id,quantity,waitlisted_quantity,is_new_cart,unit_price) VALUES($1,$2,$3,1,0,false,1000)`, id, fx.productID, fx.cartID)
	if err := testRepo.FinishCommentWork(ctx, id, owner, live.ErrCommentERPBlocked); err != nil {
		t.Fatal(err)
	}
	makeCommentRetryDue(t, id)
	if _, _, err := testRepo.BeginCommentWork(ctx, id, []byte(`{}`)); !errors.Is(err, live.ErrCommentBusy) {
		t.Fatalf("financial block retried: %v", err)
	}
	svc := newFinalisationService(newScriptedERP())
	if err := svc.ReserveStockInERP(ctx, fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, ""); !errors.Is(err, providers.ErrOrderAccountsLaunched) {
		t.Fatalf("new comment bypassed block: %v", err)
	}
	state, err := cartedit.Read(ctx, testPool, fx.cartID)
	if err != nil || !state.Pending || !state.Blocked || state.BlockReason != "accounts_launched" {
		t.Fatalf("missing merchant/checkout warning: %+v %v", state, err)
	}
	if err := cartedit.AssertReady(ctx, testPool, fx.cartID); err == nil {
		t.Fatal("checkout allowed unconfirmed item")
	}
	var quantity, confirmed int
	if err := testPool.QueryRow(ctx, `SELECT quantity,erp_confirmed_quantity FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&quantity, &confirmed); err != nil {
		t.Fatal(err)
	}
	if quantity != 2 || confirmed != 1 {
		t.Fatal("block changed quantities")
	}
	checkoutRecoveryExec(t, `UPDATE cart_erp_item_blocks SET retry_at=now()-interval '1 second' WHERE cart_id=$1`, fx.cartID)
	if _, _, err := testRepo.checkERPItemAccountsBlock(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	owner, done, err := testRepo.BeginCommentWork(ctx, id, []byte(`{}`))
	if err != nil || done || owner == "" {
		t.Fatalf("due recovery did not resume: %v", err)
	}
	checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=NULL,erp_confirmed_quantity=quantity WHERE cart_id=$1`, fx.cartID)
	state, err = cartedit.Read(ctx, testPool, fx.cartID)
	if err != nil || state.Pending || state.Blocked {
		t.Fatalf("reconciled grid remains blocked: %+v %v", state, err)
	}
	if err := testRepo.FinishCommentWork(ctx, id, owner, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccountsBlockDoesNotFollowAnotherOrderOrIntegration(t *testing.T) {
	requireDB(t)
	for _, scenario := range []string{"rebind", "inactive integration", "cancelled cart", "removed items", "foreign store"} {
		t.Run(scenario, func(t *testing.T) {
			fx := seedPaidCart(t, 1, 0)
			checkoutRecoveryExec(t, `UPDATE carts SET external_order_id='old-order',payment_status='pending' WHERE id=$1`, fx.cartID)
			checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=0 WHERE cart_id=$1`, fx.cartID)
			orderID, integrationID, err := testRepo.checkERPItemAccountsBlock(t.Context(), fx.cartID, fx.storeID)
			if err != nil {
				t.Fatal(err)
			}
			if err := testRepo.blockERPItemAccounts(t.Context(), fx.cartID, fx.storeID, orderID, integrationID); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "rebind":
				checkoutRecoveryExec(t, `UPDATE carts SET external_order_id='new-order' WHERE id=$1`, fx.cartID)
			case "inactive integration":
				checkoutRecoveryExec(t, `UPDATE integrations SET status='disconnected' WHERE id=$1`, integrationID)
			case "cancelled cart":
				checkoutRecoveryExec(t, `UPDATE carts SET status='cancelled' WHERE id=$1`, fx.cartID)
			case "removed items":
				checkoutRecoveryExec(t, `DELETE FROM cart_items WHERE cart_id=$1`, fx.cartID)
			case "foreign store":
				if _, _, err := testRepo.checkERPItemAccountsBlock(t.Context(), fx.cartID, uuid.NewString()); err == nil {
					t.Fatal("cross-store access allowed")
				}
				return
			}
			state, err := cartedit.Read(t.Context(), testPool, fx.cartID)
			if err != nil || state.Blocked {
				t.Fatalf("stale block survived %s: %+v %v", scenario, state, err)
			}
		})
	}
}

func seedTinyRecovery(t *testing.T) (finFixture, string) {
	t.Helper()
	fx := seedPaidCart(t, 1, 0)
	id := uuid.NewString()
	checkoutRecoveryExec(t, `UPDATE carts SET external_order_id='source',erp_order_state='open' WHERE id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `UPDATE order_payments p SET erp_finalisation_status='failed',erp_last_attempt_at=now()-interval '1 hour' FROM orders o WHERE o.id=p.order_id AND o.cart_id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `INSERT INTO tiny_checkout_operations(id,cart_id,integration_id,source_order_id,progress,updated_at,recovery_after) SELECT $1,$2,id,'source','{}',now()-interval '1 hour',now()-interval '1 minute' FROM integrations WHERE store_id=$3`, id, fx.cartID, fx.storeID)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM tiny_checkout_operations WHERE cart_id=$1`, fx.cartID)
	})
	return fx, id
}

func TestTinyRecoveryClaimGuards(t *testing.T) {
	requireDB(t)
	tests := []struct {
		name, change string
		want         bool
	}{
		{"pending local operation", "", true},
		{"refunded", `UPDATE carts SET payment_status='refunded' WHERE id=$1`, false},
		{"payment review", `UPDATE carts SET payment_review_required=true WHERE id=$1`, false},
		{"cancelled", `UPDATE carts SET status='cancelled' WHERE id=$1`, false},
		{"different binding", `UPDATE carts SET external_order_id='foreign-order' WHERE id=$1`, false},
		{"writer active", `UPDATE carts SET erp_order_state='mutating' WHERE id=$1`, false},
		{"disabled integration", `UPDATE integrations SET status='disconnected' WHERE store_id=(SELECT store_id FROM carts WHERE id=$1)`, false},
		{"recent manual retry", `UPDATE order_payments p SET erp_last_attempt_at=now() FROM orders o WHERE o.id=p.order_id AND o.cart_id=$1`, false},
		{"completed operation", `UPDATE tiny_checkout_operations SET completed=true WHERE cart_id=$1`, true},
		{"no local operation", `DELETE FROM tiny_checkout_operations WHERE cart_id=$1`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx, _ := seedTinyRecovery(t)
			if tt.change != "" {
				checkoutRecoveryExec(t, tt.change, fx.cartID)
			}
			got, err := testRepo.claimTinyCheckoutRecovery(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range got {
				if item.CartID == fx.cartID {
					found = true
					if item.StoreID != fx.storeID || item.Attempts != 1 {
						t.Fatalf("bad claim: %+v", item)
					}
				}
			}
			if found != tt.want {
				t.Fatalf("eligible=%v want=%v", found, tt.want)
			}
		})
	}
}

func TestTinyRecoveryClaimConcurrentAndBackoff(t *testing.T) {
	requireDB(t)
	fx, id := seedTinyRecovery(t)
	var wg sync.WaitGroup
	results := make(chan []tinyCheckoutRecovery, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() { rows, err := testRepo.claimTinyCheckoutRecovery(t.Context()); results <- rows; failures <- err })
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
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
		t.Fatalf("concurrent claims=%d", count)
	}
	checkoutRecoveryExec(t, `UPDATE tiny_checkout_operations SET recovery_after=now()-interval '1 second',recovery_attempts=100 WHERE id=$1`, id)
	if _, err := testRepo.claimTinyCheckoutRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
	var minutes float64
	if err := testPool.QueryRow(t.Context(), `SELECT EXTRACT(EPOCH FROM recovery_after-now())/60 FROM tiny_checkout_operations WHERE id=$1`, id).Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if minutes < 359 || minutes > 360 {
		t.Fatalf("backoff not bounded: %f", minutes)
	}
}

type accountsLockedERP struct {
	*scriptedERP
	attempts int
	locked   bool
}

func (p *accountsLockedERP) UpdateOrderItems(ctx context.Context, id string, items []providers.ERPOrderItem) error {
	p.attempts++
	if p.locked {
		return providers.ErrOrderAccountsLaunched
	}
	return p.scriptedERP.UpdateOrderItems(ctx, id, items)
}

func TestAccountsBlockRecordedByReservationAndConfirmedAfterRelease(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 0)
	checkoutRecoveryExec(t, `UPDATE carts SET external_order_id='account-order',payment_status='pending',erp_order_state='open',erp_order_status='aberto' WHERE id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `UPDATE cart_items SET erp_pending_since=now()-interval '1 minute',erp_confirmed_quantity=1 WHERE cart_id=$1`, fx.cartID)
	provider := &accountsLockedERP{scriptedERP: newScriptedERP(), locked: true}
	svc := newFinalisationService(provider)
	for range 3 {
		if err := svc.ReserveStockInERP(t.Context(), fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, ""); !errors.Is(err, providers.ErrOrderAccountsLaunched) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	svc.RecoverPendingERPItems(t.Context())
	if provider.attempts != 1 {
		t.Fatalf("blocked order written %d times", provider.attempts)
	}
	if provider.count("ReverseOrderStock") != 0 {
		t.Fatal("financial refusal reversed stock")
	}
	checkoutRecoveryExec(t, `UPDATE cart_erp_item_blocks SET retry_at=now()-interval '1 second' WHERE cart_id=$1`, fx.cartID)
	provider.locked = false
	svc.RecoverPendingERPItems(t.Context())
	state, err := cartedit.Read(t.Context(), testPool, fx.cartID)
	if err != nil || state.Blocked || state.Pending || provider.attempts != 2 {
		t.Fatalf("released order not reconciled: %+v attempts=%d err=%v", state, provider.attempts, err)
	}
}

type recoveryPaidERP struct {
	*scriptedERP
	journal tinyJournalFinalizer
	reject  bool
}

func (p *recoveryPaidERP) FinalizePaidCheckout(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal) (*providers.OrderResult, error) {
	if p.reject {
		p.reject = false
		return nil, providers.ErrOrderShippingConfiguration
	}
	return p.journal.FinalizePaidCheckout(ctx, op, journal)
}

func TestTinyPaidWorkerResumesFailedJournalWithoutPaymentReplay(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	checkoutRecoveryExec(t, `UPDATE carts SET external_order_id='source',erp_order_state='open' WHERE id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,1000,1000,'pix','payment-recovery',now())`, fx.cartID)
	provider := &recoveryPaidERP{scriptedERP: newScriptedERP(), reject: true}
	svc := newFinalisationService(provider)
	status := testPaymentStatus()
	status.Amount = 1000
	if err := svc.ERP().ConfirmERPOrderPayment(t.Context(), fx.cartID, fx.storeID, status); !errors.Is(err, providers.ErrOrderShippingConfiguration) {
		t.Fatalf("first attempt: %v", err)
	}
	checkoutRecoveryExec(t, `UPDATE tiny_checkout_operations SET recovery_after=now()-interval '1 minute',updated_at=now()-interval '1 hour' WHERE cart_id=$1`, fx.cartID)
	checkoutRecoveryExec(t, `UPDATE order_payments p SET erp_last_attempt_at=now()-interval '1 hour' FROM orders o WHERE o.id=p.order_id AND o.cart_id=$1`, fx.cartID)
	svc.RecoverTinyPaidCheckouts(t.Context())
	svc.RecoverTinyPaidCheckouts(t.Context())
	var finalisation string
	var payments, operations int
	var complete bool
	if err := testPool.QueryRow(t.Context(), `SELECT p.erp_finalisation_status,(SELECT count(*) FROM cart_payments WHERE cart_id=c.id),(SELECT count(*) FROM tiny_checkout_operations WHERE cart_id=c.id),(SELECT bool_and(completed) FROM tiny_checkout_operations WHERE cart_id=c.id) FROM carts c JOIN orders o ON o.cart_id=c.id JOIN order_payments p ON p.order_id=o.id WHERE c.id=$1`, fx.cartID).Scan(&finalisation, &payments, &operations, &complete); err != nil {
		t.Fatal(err)
	}
	if finalisation != "done" || payments != 1 || operations != 1 || !complete || provider.journal.calls != 1 {
		t.Fatalf("bad recovery: status=%s payments=%d operations=%d complete=%v writes=%d", finalisation, payments, operations, complete, provider.journal.calls)
	}
}
