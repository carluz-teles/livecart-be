//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/inventory"
	orderlisteners "livecart/apps/api/internal/order/listeners"
	paymentdomain "livecart/apps/api/internal/payment"
)

type expiredApprovalERP struct {
	*scriptedERP
	snapshot providers.ERPApprovalSnapshot
	fail     bool
}

func (p *expiredApprovalERP) GetOrderApprovalSnapshot(context.Context, string) (*providers.ERPApprovalSnapshot, error) {
	if p.fail {
		return nil, fmt.Errorf("approval GET temporarily unavailable")
	}
	copy := p.snapshot
	return &copy, nil
}

func (p *expiredApprovalERP) GetOrderTotal(context.Context, string) (int64, bool, error) {
	return p.snapshot.TotalCents, p.snapshot.FreightCents > 0, nil
}

func seedExpiredApproval(t *testing.T) (finFixture, *expiredApprovalERP, *Service) {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	approvalExec(t, `DELETE FROM orders WHERE cart_id=$1`, fx.cartID)
	approvalExec(t, `UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	approvalExec(t, `UPDATE carts SET payment_status='pending',paid_at=NULL,
		external_order_id='456',erp_order_state='open',erp_order_status='aberto',
		expires_at=now()-interval '1 hour' WHERE id=$1`, fx.cartID)
	result, err := testRepo.ExpireCartAndReleaseStock(t.Context(), fx.cartID, fx.storeID)
	if err != nil || !result.Eligible {
		t.Fatalf("expiry=%+v err=%v", result, err)
	}
	approvalExec(t, `UPDATE carts SET erp_order_state='cancelled',erp_order_status='aprovado' WHERE id=$1`, fx.cartID)
	fake := &expiredApprovalERP{
		scriptedERP: newScriptedERP(),
		snapshot: providers.ERPApprovalSnapshot{
			OrderID: "456", Status: providers.ERPOrderStatusAprovado, TotalCents: 1850, FreightCents: 50,
			Items: []providers.ERPOrderItem{{ProductID: "123", Quantity: 2, UnitPrice: 900}},
		},
	}
	return fx, fake, newFinalisationService(fake)
}

func approvalExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredTinyApprovalKeepsSaleAndHistoryWithoutAnotherERPWrite(t *testing.T) {
	fx, fake, svc := seedExpiredApproval(t)
	// A new offer already exists for the same buyer. Recover the old sale
	// directly as paid, without colliding with or consuming the new offer.
	approvalExec(t, `INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,status)
		SELECT event_id,store_id,platform_user_id,platform_handle,token||'-new',short_id+100000,'active'
		FROM carts WHERE id=$1`, fx.cartID)
	for n := range 2 {
		marked, err := svc.MarkCartPaidFromERP(t.Context(), fx.cartID, fx.storeID, 1000)
		if err != nil || marked != (n == 0) {
			t.Fatalf("attempt %d marked=%v err=%v", n, marked, err)
		}
	}
	var status, payment, state, binding, reason string
	var amount int64
	var noExpiry bool
	if err := testPool.QueryRow(t.Context(), `SELECT status,payment_status,erp_order_state,
		external_order_id,cancellation_reverted_reason,paid_amount_cents,expires_at IS NULL
		FROM carts WHERE id=$1`, fx.cartID).Scan(&status, &payment, &state, &binding, &reason, &amount, &noExpiry); err != nil {
		t.Fatal(err)
	}
	if status != "checkout" || payment != "paid" || state != "confirmed" || binding != "456" ||
		reason != tinyApprovalAfterExpiry || amount != 1850 || !noExpiry {
		t.Fatalf("incorrect recovery: %s %s %s %s %s amount=%d noExpiry=%v", status, payment, state, binding, reason, amount, noExpiry)
	}
	var qty, paidQty, price int
	if err := testPool.QueryRow(t.Context(), `SELECT quantity,paid_quantity,unit_price FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&qty, &paidQty, &price); err != nil {
		t.Fatal(err)
	}
	if qty != 2 || paidQty != 2 || price != 900 {
		t.Fatalf("sale composition not reflected: qty=%d paid=%d price=%d", qty, paidQty, price)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM cart_payments WHERE cart_id=$1`, fx.cartID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger count=%d err=%v", count, err)
	}
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE name='cart.paid'
		AND payload->>'cart_id'=$1`, fx.cartID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fact struct {
		Snapshot json.RawMessage `json:"payment_snapshot"`
	}
	if err := json.Unmarshal(raw, &fact); err != nil {
		t.Fatal(err)
	}
	listener := orderlisteners.New(testPool, testRepo.queries, zap.NewNop())
	for range 2 {
		if err := listener.OnCartPaid(t.Context(), fx.cartID, fx.storeID, 0, fact.Snapshot); err != nil {
			t.Fatal(err)
		}
		if err := svc.ERP().OnOrderPaid(t.Context(), fx.cartID, fx.storeID, fact.Snapshot); err != nil {
			t.Fatal(err)
		}
		if err := svc.ERP().OnCartExpired(t.Context(), fx.cartID, fx.storeID); err != nil {
			t.Fatal(err)
		}
	}
	var total, freight, paidTotal int64
	var previousPrice int
	var finalisation, previousStatus string
	if err := testPool.QueryRow(t.Context(), `SELECT o.total_cents,o.shipping_cents,o.paid_total_cents,
		op.erp_finalisation_status,oe.metadata->'tiny_approved_after_expiry'->>'previous_status',
		(oe.metadata->'tiny_approved_after_expiry'->'previous_items'->0->>'unit_price_cents')::int
		FROM orders o JOIN order_payments op ON op.order_id=o.id JOIN order_events oe ON oe.order_id=o.id
		WHERE o.cart_id=$1 AND oe.event_type='payment_confirmed'`, fx.cartID).
		Scan(&total, &freight, &paidTotal, &finalisation, &previousStatus, &previousPrice); err != nil {
		t.Fatal(err)
	}
	if total != 1800 || freight != 50 || paidTotal != 1850 || finalisation != "done" || previousStatus != "expired" || previousPrice != 1000 {
		t.Fatalf("materialization lost sale/history: total=%d freight=%d paid=%d final=%s previous=%s", total, freight, paidTotal, finalisation, previousStatus)
	}
	if len(fake.calls) > 0 {
		t.Fatalf("recovery or old expiry wrote to ERP: %v", fake.calls)
	}
	var source, kind string
	if err := testPool.QueryRow(t.Context(), `SELECT source,payload->>'kind' FROM event_outbox
		WHERE dedup_key=$1`, "tiny.expired-approval.stock:"+fx.cartID+":"+fx.productID).Scan(&source, &kind); err != nil {
		t.Fatal(err)
	}
	if source != string(events.SourceTiny) || kind != "estoque" || productStock(t, fx.productID) != 0 {
		t.Fatalf("stock reconciliation not fenced/enqueued: source=%s kind=%s", source, kind)
	}
}

func TestExpiredTinyApprovalPreservesGuards(t *testing.T) {
	for _, scenario := range []string{"cancelled", "refunded", "paid", "another store", "bling", "remote cancelled", "remote draft", "wrong sale", "pending edit", "read failure", "review", "existing order"} {
		t.Run(scenario, func(t *testing.T) {
			fx, fake, svc := seedExpiredApproval(t)
			storeID := fx.storeID
			wantErr := false
			switch scenario {
			case "cancelled":
				approvalExec(t, `UPDATE carts SET status='cancelled',cancelled_reason='store_cancelled' WHERE id=$1`, fx.cartID)
			case "refunded", "paid":
				approvalExec(t, `UPDATE carts SET payment_status=$2 WHERE id=$1`, fx.cartID, scenario)
			case "another store":
				storeID = seedPaidCart(t, 1, 0).storeID
				wantErr = true
			case "bling":
				approvalExec(t, `UPDATE integrations SET provider='bling' WHERE store_id=$1`, fx.storeID)
			case "remote cancelled":
				fake.snapshot.Status = providers.ERPOrderStatusCancelado
			case "remote draft":
				fake.snapshot.Status = providers.ERPOrderStatusAberto
			case "wrong sale":
				fake.snapshot.OrderID = "789"
				wantErr = true
			case "pending edit":
				approvalExec(t, `INSERT INTO cart_erp_edits(cart_id,revision) VALUES($1,1)`, fx.cartID)
				wantErr = true
			case "read failure":
				fake.fail, wantErr = true, true
			case "review":
				approvalExec(t, `UPDATE carts SET payment_review_required=true WHERE id=$1`, fx.cartID)
				wantErr = true
			case "existing order":
				approvalExec(t, `INSERT INTO orders(cart_id,short_id,store_id,event_id,status)
					SELECT id,short_id,store_id,event_id,'paid' FROM carts WHERE id=$1`, fx.cartID)
				wantErr = true
			}
			marked, err := svc.MarkCartPaidFromERP(t.Context(), fx.cartID, storeID, 1000)
			if marked || (err != nil) != wantErr {
				t.Fatalf("guard marked=%v err=%v wantErr=%v", marked, err, wantErr)
			}
			var count int
			if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM cart_payments WHERE cart_id=$1`, fx.cartID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("guard created payment count=%d err=%v", count, err)
			}
		})
	}
}

func TestExpiredTinyApprovalRecoversLinkedPurchaseAndKeepsOrigins(t *testing.T) {
	fx, fake, svc := seedExpiredApproval(t)
	var child string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO carts(event_id,store_id,platform_user_id,
		platform_handle,token,short_id,status,payment_status,joined_to_cart_id,purchase_closed,erp_order_state)
		SELECT event_id,store_id,'approval-child','approval-child','approval-child-'||id::text,
		short_id+100000,'expired','pending',id,true,'none' FROM carts WHERE id=$1 RETURNING id::text`, fx.cartID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	approvalExec(t, `INSERT INTO cart_items(cart_id,product_id,quantity,unit_price) VALUES($1,$2,1,1000)`, child, fx.productID)
	fake.snapshot.Items[0].Quantity, fake.snapshot.TotalCents = 3, 2750
	fake.snapshot.Status, fake.snapshot.InvoiceID = providers.ERPOrderStatusFaturado, "99"
	if marked, err := svc.MarkCartPaidFromERP(t.Context(), child, fx.storeID, 1000); err != nil || !marked {
		t.Fatalf("approval through source cart: marked=%v err=%v", marked, err)
	}
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload->'payment_snapshot' FROM event_outbox
		WHERE name='cart.paid' AND payload->>'cart_id'=$1`, fx.cartID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	listener := orderlisteners.New(testPool, testRepo.queries, zap.NewNop())
	if err := listener.OnCartPaid(t.Context(), fx.cartID, fx.storeID, 0, raw); err != nil {
		t.Fatal(err)
	}
	var gmv, paid, covered int
	if err := testPool.QueryRow(t.Context(), `SELECT o.total_cents,o.paid_total_cents,p.gross_covered_cents
		FROM orders o JOIN cart_payments p ON p.cart_id=o.cart_id WHERE o.cart_id=$1`, fx.cartID).
		Scan(&gmv, &paid, &covered); err != nil {
		t.Fatal(err)
	}
	if gmv != 2700 || paid != 2750 || covered != 2750 {
		t.Fatalf("group money lost: gmv=%d paid=%d covered=%d", gmv, paid, covered)
	}
	var qty, paidQty, childPayments, statusObservations int
	var closed bool
	if err := testPool.QueryRow(t.Context(), `SELECT ci.quantity,ci.paid_quantity,c.purchase_closed,
		(SELECT count(*) FROM cart_payments WHERE cart_id=c.id),
		(SELECT count(*) FROM erp_order_status_events WHERE cart_id=$2 AND status='faturado' AND source='reconciliation')
		FROM carts c JOIN cart_items ci ON ci.cart_id=c.id WHERE c.id=$1`, child, fx.cartID).
		Scan(&qty, &paidQty, &closed, &childPayments, &statusObservations); err != nil {
		t.Fatal(err)
	}
	if qty != 1 || paidQty != 1 || !closed || childPayments != 0 || statusObservations != 1 {
		t.Fatalf("source/history lost: qty=%d paid=%d closed=%v childPayments=%d statusEvents=%d", qty, paidQty, closed, childPayments, statusObservations)
	}
}

func TestExpiredTinyApprovalRollsBackAndRetries(t *testing.T) {
	fx, _, svc := seedExpiredApproval(t)
	approvalExec(t, fmt.Sprintf(`CREATE FUNCTION approval_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.id='%s'::uuid THEN RAISE EXCEPTION 'injected approval failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER approval_failure BEFORE UPDATE OF stock ON products FOR EACH ROW EXECUTE FUNCTION approval_failure()`, fx.productID))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS approval_failure ON products; DROP FUNCTION IF EXISTS approval_failure()`)
	})
	if marked, err := svc.MarkCartPaidFromERP(t.Context(), fx.cartID, fx.storeID, 1000); err == nil || marked {
		t.Fatalf("injected failure committed marked=%v err=%v", marked, err)
	}
	var qty, count int
	if err := testPool.QueryRow(t.Context(), `SELECT quantity,(SELECT count(*) FROM cart_payments WHERE cart_id=$1)
		FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&qty, &count); err != nil {
		t.Fatal(err)
	}
	if qty != 1 || count != 0 || cartStatus(t, fx.cartID) != "expired" {
		t.Fatal("failed recovery committed a partial sale")
	}
	approvalExec(t, `DROP TRIGGER approval_failure ON products; DROP FUNCTION approval_failure()`)
	if marked, err := svc.MarkCartPaidFromERP(t.Context(), fx.cartID, fx.storeID, 1000); err != nil || !marked {
		t.Fatalf("retry marked=%v err=%v", marked, err)
	}
}

func TestExpiredTinyApprovalConcurrentDeliveryRecordsOnePayment(t *testing.T) {
	fx, _, svc := seedExpiredApproval(t)
	var marked atomic.Int32
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			ok, err := svc.MarkCartPaidFromERP(t.Context(), fx.cartID, fx.storeID, 1000)
			if err != nil && !errors.Is(err, erp.ErrCartBusy) {
				t.Errorf("concurrent recovery: %v", err)
			}
			if ok {
				marked.Add(1)
			}
		}()
	}
	group.Wait()
	if marked.Load() != 1 {
		t.Fatalf("marked %d times", marked.Load())
	}
}

func TestExpiredGatewayPaymentDoesNotUseTinyApprovalException(t *testing.T) {
	fx, _, _ := seedExpiredApproval(t)
	_, err := testRepo.UpdateCartPaymentStatus(t.Context(), fx.cartID, "paid", "gateway-payment", nil, "pix", 1000)
	if !errors.Is(err, paymentdomain.ErrCartNotPayable) {
		t.Fatalf("gateway bypassed expiry: %v", err)
	}
}

func TestExpiredTinyApprovalRetriesFromRecordedApprovalAndRefreshesAvailableStock(t *testing.T) {
	fx, _, svc := seedExpiredApproval(t)
	row, err := testRepo.GetActiveERP(t.Context(), fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	command := TinyApprovalCommand{StoreID: fx.storeID, IntegrationID: row.ID, OrderID: "456", Kind: "order_approved"}
	for range 2 {
		if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	var stockCommand TinyProductWebhookCommand
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE dedup_key=$1`,
		"tiny.expired-approval.stock:"+fx.cartID+":"+fx.productID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &stockCommand); err != nil {
		t.Fatal(err)
	}
	// Read a current available balance AFTER Tiny has accounted for the sale.
	// The new reader must not subtract the two sold units a second time.
	stockSvc := reconnectTestService(t)
	encrypted, err := stockSvc.encryptor.EncryptJSON(providers.Credentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	approvalExec(t, `UPDATE integrations SET credentials=$2 WHERE id=$1`, row.ID, encrypted)
	reads := 0
	stockSvc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { reads++; return 5, nil }}, nil
	}})
	// A stock task may arrive before order.paid. The snapshot is saved, while
	// waitlist promotion correctly retries until the payment reactor finishes.
	if err := stockSvc.ProcessTinyProductWebhook(t.Context(), stockCommand); !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) {
		t.Fatalf("stock should await order materialization, got %v", err)
	}
	if err := testPool.QueryRow(t.Context(), `SELECT payload->'payment_snapshot' FROM event_outbox
		WHERE name='cart.paid' AND payload->>'cart_id'=$1`, fx.cartID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	listener := orderlisteners.New(testPool, testRepo.queries, zap.NewNop())
	if err := listener.OnCartPaid(t.Context(), fx.cartID, fx.storeID, 0, raw); err != nil {
		t.Fatal(err)
	}
	if err := svc.ERP().OnOrderPaid(t.Context(), fx.cartID, fx.storeID, raw); err != nil {
		t.Fatal(err)
	}
	if err := stockSvc.ProcessTinyProductWebhook(t.Context(), stockCommand); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || productStock(t, fx.productID) != 5 {
		t.Fatalf("available stock deducted twice or event replayed: reads=%d stock=%d", reads, productStock(t, fx.productID))
	}
}
