package integration

import (
	"context"
	"encoding/json"
	"testing"
)

// Fatia 11b-2: the ERP finalisation/refund moved off cart.paid/cart.refunded onto
// the order.paid/order.refunded facts. These tests exercise the two new reactors
// (ReactOrderPaidERP / ReactOrderRefundedERP) end to end against the scripted ERP,
// proving the trigger inversion kept the finalisation/refund behaviour intact and
// that the writes land in order_payments (resolved via orders.cart_id).

// TestReactOrderPaidERP_FinalisesAndFindsOrder proves AC1: consuming order.paid
// runs the ERP finalisation and finds the materialised Order, so the finalisation
// markers/snapshot are written authoritatively to order_payments.
func TestReactOrderPaidERP_FinalisesAndFindsOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	snapshot, err := json.Marshal(testPaymentStatus())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	if err := svc.ReactOrderPaidERP(context.Background(), fx.cartID, fx.storeID, snapshot); err != nil {
		t.Fatalf("ReactOrderPaidERP: %v", err)
	}

	// The Order was found (writes resolved via orders.cart_id) → order_payments holds
	// the authoritative finalisation state + snapshot.
	status, _, externalOrderID, attempts, hasSnapshot := cartFinalisationState(t, fx.cartID)
	if status != "done" {
		t.Fatalf("erp_finalisation_status = %q, esperado done", status)
	}
	if externalOrderID != "ORD-1" {
		t.Fatalf("external_order_id = %q, esperado ORD-1", externalOrderID)
	}
	if attempts != 1 || !hasSnapshot {
		t.Fatalf("attempts=%d hasSnapshot=%v — finalisation não escreveu em order_payments", attempts, hasSnapshot)
	}
	if fake.count("CreateOrder") != 1 || fake.count("Launch") != 1 {
		t.Fatalf("finalisation não rodou no ERP: %v", fake.calls)
	}
}

// TestReactOrderPaidERP_NoERPIntegrationNoOps proves the reactor no-ops (returns
// nil, touches no ERP) when the store has no active Tiny integration — a paid order
// must never churn asynq retries just because the store never wired an ERP.
func TestReactOrderPaidERP_NoERPIntegrationNoOps(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM integrations WHERE store_id = $1 AND type = 'erp'`, fx.storeID); err != nil {
		t.Fatalf("remove integration: %v", err)
	}

	if err := svc.ReactOrderPaidERP(context.Background(), fx.cartID, fx.storeID, nil); err != nil {
		t.Fatalf("ReactOrderPaidERP sem integração deveria no-op: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("no-op tocou o ERP: %v", fake.calls)
	}
	status, _, _, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "pending" {
		t.Fatalf("finalisation status = %q, esperado pending (intocado)", status)
	}
}

// TestReactOrderRefundedERP_CancelsConvertedOrder proves AC2: consuming
// order.refunded cancels the converted ERP order and returns its stock via
// RefundConvertedCartOrder — the same behaviour that used to run inline in
// ReactCartRefunded, now triggered by the order.refunded fact.
func TestReactOrderRefundedERP_CancelsConvertedOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := svc.ConfirmERPOrderPayment(context.Background(), fx.cartID, fx.storeID, testPaymentStatus()); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if err := svc.ReactOrderRefundedERP(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ReactOrderRefundedERP: %v", err)
	}

	iSit, iRev := fake.firstIndex("Situacao:2"), fake.firstIndex("OrderReverse")
	if !(iSit >= 0 && iRev > iSit) {
		t.Fatalf("refund deve cancelar e SÓ ENTÃO estornar: %v", fake.calls)
	}
	if state, _, _ := cartOrderState(t, fx.cartID); state != "cancelled" {
		t.Fatalf("state pós-refund = %q, esperado cancelled", state)
	}
}
