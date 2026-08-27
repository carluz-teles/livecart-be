package integration

import (
	"context"
	"encoding/json"
	"testing"
)

// Os reactors de ERP consomem os fatos order.paid / order.refunded (e não
// cart.paid / cart.refunded), o que desacopla o laço de retry do ERP do fan-out
// para o comprador. Estes testes os exercitam de ponta a ponta contra o ERP
// roteirizado, contra um Postgres de verdade.

// order.paid cria o pedido no ERP (o carrinho aqui nasceu pago, sem passar pela
// live) e o aprova, escrevendo os marcadores em order_payments.
func TestReactOrderPaidERP_FinalisesAndFindsOrder(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	snapshot, err := json.Marshal(testPaymentStatus())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	if err := svc.ERP().OnOrderPaid(context.Background(), fx.cartID, fx.storeID, snapshot); err != nil {
		t.Fatalf("ReactOrderPaidERP: %v", err)
	}

	// The Order was found (writes resolved via orders.cart_id) → order_payments holds
	// the authoritative finalisation state + snapshot.
	status, _, externalOrderID, attempts, hasSnapshot := cartFinalisationState(t, fx.cartID)
	if status != "done" {
		t.Fatalf("erp_finalisation_status = %q, esperado done", status)
	}
	if externalOrderID == "" {
		t.Fatal("external_order_id vazio — o pedido não foi gravado no carrinho")
	}
	if attempts != 1 || !hasSnapshot {
		t.Fatalf("attempts=%d hasSnapshot=%v — finalisation não escreveu em order_payments", attempts, hasSnapshot)
	}
	if fake.count("CreateOrder") != 1 {
		t.Fatalf("pedidos criados = %d, quero 1: %v", fake.count("CreateOrder"), fake.calls)
	}
	if fake.count("Reverse:") != 0 {
		t.Fatalf("estornou estoque no caminho pago: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("criou %d reserva(s) manual(is) — o pedido é a reserva", n)
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

	if err := svc.ERP().OnOrderPaid(context.Background(), fx.cartID, fx.storeID, nil); err != nil {
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

// order.refunded cancela o pedido. UMA chamada: cancelar já devolve a reserva, e
// acompanhá-la de um estorno a INFLARIA.
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

	if err := svc.ERP().OnOrderRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("ReactOrderRefundedERP: %v", err)
	}

	if fake.count("Reverse:") != 0 {
		t.Fatalf("o refund estornou estoque: %v — num pedido que só reservou, "+
			"estornar infla a reserva em vez de devolvê-la", fake.calls)
	}
	if n := len(fake.callsWithPrefix("Situacao:")); n == 0 {
		t.Fatalf("o refund não cancelou o pedido: %v", fake.calls)
	}
	if state, _, _, _ := cartERPState(t, fx.cartID); state != "cancelled" {
		t.Fatalf("state pós-refund = %q, esperado cancelled", state)
	}
}
