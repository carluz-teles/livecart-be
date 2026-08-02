package integration

import (
	"context"
	"encoding/json"
	"testing"
)

// Bloco B2c-2 — AC1: os reactors ERP (agora com corpo em internal/erp, chamados
// via delegação React*ERP) são idempotentes sob redelivery do asynq. Uma segunda
// entrega do MESMO fato não pode duplicar pedido, baixa/entrada de estoque nem
// estorno. Rodam contra o Postgres real + ERP roteirizado (mesma infra do
// finalisation_resume_test).

// AC1a: OnOrderPaid 2× → erp_finalisation_status='done', CreateOrder==1,
// Launch==1. A redelivery cai no guard de done e não custa nem uma chamada ao
// Tiny.
func TestReactOrderPaidERP_RedeliveryIsIdempotent(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 2, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	snapshot, err := json.Marshal(testPaymentStatus())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := svc.ERP().OnOrderPaid(context.Background(), fx.cartID, fx.storeID, snapshot); err != nil {
			t.Fatalf("ReactOrderPaidERP #%d: %v", i+1, err)
		}
	}

	status, _, orderID, _, _ := cartFinalisationState(t, fx.cartID)
	if status != "done" || orderID != "ORD-1" {
		t.Fatalf("status=%q orderID=%q — esperado done/ORD-1", status, orderID)
	}
	if fake.count("CreateOrder") != 1 || fake.count("Launch") != 1 {
		t.Fatalf("redelivery duplicou o pedido/launch: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active pós-redelivery = %d", n)
	}
}

// AC1b: OnOrderRefunded 2× → state=cancelled, uma única situação 2 (cancelar) e
// um único estorno do pedido. A segunda entrega vê 'cancelled' e no-opa.
func TestReactOrderRefundedERP_RedeliveryIsIdempotent(t *testing.T) {
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

	for i := 0; i < 2; i++ {
		if err := svc.ERP().OnOrderRefunded(context.Background(), fx.cartID, fx.storeID); err != nil {
			t.Fatalf("ReactOrderRefundedERP #%d: %v", i+1, err)
		}
	}

	if state, _, _ := cartOrderState(t, fx.cartID); state != "cancelled" {
		t.Fatalf("state pós-refund = %q, esperado cancelled", state)
	}
	if c := fake.count("Situacao:2"); c != 1 {
		t.Fatalf("cancelamento (situação 2) rodou %d× — esperado 1", c)
	}
	if c := fake.count("OrderReverse"); c != 1 {
		t.Fatalf("estorno do pedido rodou %d× — esperado 1", c)
	}
}

// AC1c: OnCartExpired 2× (cart NÃO convertido) → reservas revertidas sem estorno
// duplicado. A segunda entrega não encontra reservas 'active' e no-opa no Tiny.
func TestReactCartExpiredERP_RedeliveryIsIdempotent(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	for i := 0; i < 2; i++ {
		if err := svc.ERP().OnCartExpired(context.Background(), fx.cartID, fx.storeID); err != nil {
			t.Fatalf("ReactCartExpiredERP #%d: %v", i+1, err)
		}
	}

	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas active pós-expiração = %d", n)
	}
	if c := fake.count("ReverseRes"); c != 1 {
		t.Fatalf("estorno de reserva rodou %d× — esperado 1 (sem duplicar na redelivery): %v", c, fake.calls)
	}
	if fake.count("CreateOrder") != 0 || fake.count("OrderReverse") != 0 {
		t.Fatalf("expiração de cart não convertido não pode criar/cancelar pedido: %v", fake.calls)
	}
}
