package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// Os reactors ERP são idempotentes sob redelivery do asynq. Uma segunda entrega
// do MESMO fato não pode duplicar pedido nem movimentar estoque. Rodam contra o
// Postgres real + ERP roteirizado.

// OnOrderPaid 2× → um pedido, uma aprovação. A redelivery cai no guard de
// 'confirmed' e não custa nem uma chamada ao ERP.
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
	if status != "done" || orderID == "" {
		t.Fatalf("status=%q orderID=%q — esperado done com pedido gravado", status, orderID)
	}
	if fake.count("CreateOrder") != 1 {
		t.Fatalf("redelivery duplicou o pedido: %v", fake.calls)
	}
	if fake.count("Reverse:") != 0 {
		t.Fatalf("redelivery estornou estoque: %v", fake.calls)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas manuais = %d, quero 0", n)
	}
}

// OnOrderRefunded 2× → state=cancelled com UM cancelamento e ZERO estornos. A
// segunda entrega vê 'cancelled' e no-opa.
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

	if state, _, _, _ := cartERPState(t, fx.cartID); state != "cancelled" {
		t.Fatalf("state pós-refund = %q, esperado cancelled", state)
	}
	cancelamentos := 0
	for _, c := range fake.callsWithPrefix("Situacao:") {
		if strings.HasSuffix(c, fmt.Sprintf(":%d", providers.SituacaoCancelada)) {
			cancelamentos++
		}
	}
	if cancelamentos != 1 {
		t.Fatalf("cancelamento rodou %d× — esperado 1: %v", cancelamentos, fake.calls)
	}
	if c := fake.count("Reverse:"); c != 0 {
		t.Fatalf("estorno rodou %d× — esperado 0: cancelar já devolve a reserva, e "+
			"estornar por cima a inflaria", c)
	}
}

// OnCartExpired 2× num carrinho COM pedido → um cancelamento, zero estornos. A
// segunda entrega vê 'cancelled' e no-opa.
func TestReactCartExpiredERP_RedeliveryIsIdempotent(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.EnsureERPOrderForCart(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("criando pedido: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := svc.ERP().OnCartExpired(context.Background(), fx.cartID, fx.storeID); err != nil {
			t.Fatalf("ReactCartExpiredERP #%d: %v", i+1, err)
		}
	}

	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reservas manuais = %d, quero 0", n)
	}
	if c := fake.count("Reverse:"); c != 0 {
		t.Fatalf("estorno rodou %d× na expiração — esperado 0: %v", c, fake.calls)
	}
	cancelamentos := 0
	for _, c := range fake.callsWithPrefix("Situacao:") {
		if strings.HasSuffix(c, fmt.Sprintf(":%d", providers.SituacaoCancelada)) {
			cancelamentos++
		}
	}
	if cancelamentos != 1 {
		t.Fatalf("cancelamento rodou %d× — esperado 1: %v", cancelamentos, fake.calls)
	}
}

// Carrinho SEM pedido nenhum expira sem falar com o ERP.
func TestReactCartExpiredERP_SemPedidoNaoFalaComOERP(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	fake := newScriptedERP()
	svc := newFinalisationService(fake)

	if err := svc.ERP().OnCartExpired(context.Background(), fx.cartID, fx.storeID); err != nil {
		t.Fatalf("expiração: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("falou com o ERP sem ter pedido: %v", fake.calls)
	}
}
