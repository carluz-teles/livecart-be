package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	orderlisteners "livecart/apps/api/internal/order/listeners"
)

func TestERPPaymentMaterializesOrderExactlyOnceWithoutWritingBackToERP(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := t.Context()
	if _, err := testPool.Exec(ctx, `DELETE FROM orders WHERE cart_id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET payment_status='pending',paid_at=NULL,external_order_id='test-approved-order' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	fake := newScriptedERP()
	svc := newFinalisationService(fake)
	for i := 0; i < 2; i++ {
		marked, err := svc.MarkCartPaidFromERP(ctx, fx.cartID, fx.storeID, 1000)
		if err != nil || marked != (i == 0) {
			t.Fatalf("attempt %d marked=%v err=%v", i, marked, err)
		}
	}
	var count int
	var payload []byte
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE name=$1 AND payload->>'cart_id'=$2`, events.CartPaid, fx.cartID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("paid events=%d err=%v", count, err)
	}
	if err := testPool.QueryRow(ctx, `SELECT payload FROM event_outbox WHERE name=$1 AND payload->>'cart_id'=$2`, events.CartPaid, fx.cartID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var paid struct {
		Snapshot json.RawMessage `json:"payment_snapshot"`
	}
	if err := json.Unmarshal(payload, &paid); err != nil {
		t.Fatal(err)
	}
	if !providers.IsERPRecordedPaymentSnapshot(paid.Snapshot) {
		t.Fatal("lost ERP-origin acknowledgment")
	}
	listener := orderlisteners.New(testPool, testRepo.queries, zap.NewNop())
	for range 2 {
		if err := listener.OnCartPaid(ctx, fx.cartID, fx.storeID, 0, paid.Snapshot); err != nil {
			t.Fatal(err)
		}
		if err := svc.ERP().OnOrderPaid(ctx, fx.cartID, fx.storeID, paid.Snapshot); err != nil {
			t.Fatal(err)
		}
	}
	var status, finalisation string
	if err := testPool.QueryRow(ctx, `SELECT op.payment_status,op.erp_finalisation_status FROM orders o JOIN order_payments op ON op.order_id=o.id WHERE o.cart_id=$1`, fx.cartID).Scan(&status, &finalisation); err != nil {
		t.Fatal(err)
	}
	if status != "paid" || finalisation != "done" {
		t.Fatalf("payment=%s finalisation=%s", status, finalisation)
	}
	if len(fake.calls) > 0 {
		t.Fatalf("ERP-origin payment wrote back to provider: %+v", fake.calls)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM cart_payments WHERE cart_id=$1`, fx.cartID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("payments=%d err=%v", count, err)
	}
}

func TestTinyApprovalCommandIsDurableAndBoundToItsIntegration(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	svc := newFinalisationService(newScriptedERP())
	if err := svc.enqueueTinyApproval(t.Context(), fx.storeID, "approval-fixture"); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`, events.ERPWebhookProcess, fx.storeID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var command TinyApprovalCommand
	if err := json.Unmarshal(payload, &command); err != nil {
		t.Fatal(err)
	}
	if command.OrderID != "approval-fixture" || command.IntegrationID == "" {
		t.Fatalf("lost command: %+v", command)
	}
	// Orders from other channels are ignored without retrying provider calls.
	if err := svc.ProcessTinyApproval(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	command.IntegrationID = "replaced"
	if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
		t.Fatal(err)
	}
}

func TestTinyOAuthCanFindAnErroredIntegration(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET status='error' WHERE store_id=$1`, fx.storeID); err != nil {
		t.Fatal(err)
	}
	svc := newFinalisationService(newScriptedERP())
	row, err := svc.tinyOAuthIntegration(t.Context(), fx.storeID)
	if err != nil || row == nil || row.Provider != "tiny" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET provider='bling' WHERE store_id=$1`, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.tinyOAuthIntegration(t.Context(), fx.storeID); err == nil {
		t.Fatal("Tiny OAuth adopted another ERP")
	}
}

type approvalTotalReader struct {
	providers.ERPProvider
	fail bool
}

func (p *approvalTotalReader) GetOrderTotal(context.Context, string) (int64, bool, error) {
	if p.fail {
		return 0, false, fmt.Errorf("temporary Tiny outage")
	}
	return 1000, true, nil
}

func TestTinyApprovalRetriesEvenAfterTheOrderWasInvoiced(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',paid_at=NULL,external_order_id='approved-then-invoiced',erp_order_status='faturado' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var integrationID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	reader := &approvalTotalReader{fail: true}
	svc := newFinalisationService(reader)
	command := TinyApprovalCommand{StoreID: fx.storeID, IntegrationID: integrationID, OrderID: "approved-then-invoiced", Kind: "order_approved"}
	if err := svc.ProcessTinyApproval(t.Context(), command); err == nil {
		t.Fatal("lost transient failure")
	}
	reader.fail = false
	if err := svc.ProcessTinyApproval(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	var payment, situation string
	if err := testPool.QueryRow(t.Context(), `SELECT payment_status,erp_order_status FROM carts WHERE id=$1`, fx.cartID).Scan(&payment, &situation); err != nil {
		t.Fatal(err)
	}
	if payment != "paid" || situation != "faturado" {
		t.Fatalf("payment=%s situation=%s", payment, situation)
	}
}
