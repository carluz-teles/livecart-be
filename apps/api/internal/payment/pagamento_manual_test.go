package payment_test

// Pagamento recebido por fora, confirmado à mão pelo lojista.
//
// A regra que carrega a feature é que o ciclo é o MESMO: a confirmação manual
// aplica a escrita guardada e emite `cart.paid`, e é aquele fato que materializa
// a Order, cria o pedido no ERP e lança o estoque. Um atalho que marcasse o
// carrinho como pago sem emitir o fato deixaria a venda sem pedido no Tiny — o
// oposto exato do que o lojista pediu.
//
// Invariantes:
//   N1 emite cart.paid, com dedup DETERMINÍSTICO (dois cliques = um ciclo)
//   N2 NÃO manda snapshot de pagamento — é ele que vira contas a receber no
//      Tiny, e o lojista lança isso lá
//   N3 pedido já pago é RECUSADO: o dedup do gateway não bate com o sintético,
//      então o fan-out rodaria de novo sobre uma venda fechada
//   N4 pedido sem itens é recusado antes de marcar pago
//   N5 carrinho cancelado pela loja é restaurado (o dinheiro manda); expirado é
//      recusado com instrução, não marcado às escondidas

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
)

const (
	cartManual  = "8f1c2f7e-0000-0000-0000-000000000001"
	storeManual = "a5403331-afd4-40ff-9bbe-4aa6f5322aee"
)

func gatewayPronto() *mockGateway {
	gw := newMockGateway()
	gw.cartStatus = "pending"
	gw.gmv = 12500
	return gw
}

// ─── N1 + N2 ────────────────────────────────────────────────────────────────

func TestPagamentoManual_EmiteOMesmoFatoDoPagamentoNormal(t *testing.T) {
	gw := gatewayPronto()
	svc := newWebhookService(gw)

	if err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual); err != nil {
		t.Fatalf("ConfirmManualPayment: %v", err)
	}

	if len(gw.updateCalls) != 1 || gw.updateCalls[0] != "paid" {
		t.Fatalf("escrita de pagamento = %v, esperava um único \"paid\"", gw.updateCalls)
	}

	env, ok := gw.emittedByKey[string(events.CartPaid)+":manual:"+cartManual]
	if !ok {
		t.Fatalf("cart.paid não foi emitido — sem esse fato a venda não vira Order "+
			"nem chega ao ERP, que é justamente o que o lojista pediu. Emitidos: %v",
			gw.emitOrder)
	}
	if env.Name != events.CartPaid {
		t.Errorf("fato emitido = %q, esperava cart.paid", env.Name)
	}

	var payload struct {
		CartID          string          `json:"cart_id"`
		StoreID         string          `json:"store_id"`
		PaymentID       string          `json:"payment_id"`
		Method          string          `json:"payment_method"`
		GMVCents        int64           `json:"gmv_cents"`
		PaymentSnapshot json.RawMessage `json:"payment_snapshot"`
	}
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("payload ilegível: %v (%s)", err, env.Payload)
	}
	if payload.CartID != cartManual || payload.StoreID != storeManual {
		t.Errorf("payload aponta para outro carrinho/loja: %+v", payload)
	}
	if payload.GMVCents != 12500 {
		t.Errorf("gmv_cents = %d, esperava 12500 — é o valor que vira a Order", payload.GMVCents)
	}
	if payload.Method != "manual" {
		t.Errorf("payment_method = %q; sem a marca, um pagamento por fora fica "+
			"indistinguível de um do gateway na conferência do caixa", payload.Method)
	}

	// ─── N2 ─────────────────────────────────────────────────────────────────
	if len(payload.PaymentSnapshot) != 0 && string(payload.PaymentSnapshot) != "null" {
		t.Errorf("foi junto um snapshot de pagamento (%s) — é ele que vira contas a "+
			"receber no Tiny, e o lojista pediu para lançar isso lá",
			payload.PaymentSnapshot)
	}
}

// Dois cliques têm de virar UM ciclo. O dedup é derivado do payment_id, que é
// determinístico por carrinho — se ele carregasse timestamp ou aleatório, o
// segundo clique criaria um segundo pedido no ERP.
func TestPagamentoManual_DedupEhDeterministico(t *testing.T) {
	gw := gatewayPronto()
	svc := newWebhookService(gw)
	ctx := context.Background()

	if err := svc.ConfirmManualPayment(ctx, cartManual, storeManual); err != nil {
		t.Fatalf("primeira confirmação: %v", err)
	}
	primeira := gw.emitOrder[len(gw.emitOrder)-1]

	// Segunda chamada com o carrinho AINDA pendente (o mock não muda de estado):
	// o que precisa colidir é a chave, não o estado.
	if err := svc.ConfirmManualPayment(ctx, cartManual, storeManual); err != nil {
		t.Fatalf("segunda confirmação: %v", err)
	}
	segunda := gw.emitOrder[len(gw.emitOrder)-1]

	if primeira != segunda {
		t.Errorf("dedup_key mudou entre os cliques (%q → %q) — o fan-out rodaria "+
			"duas vezes e criaria um segundo pedido no ERP", primeira, segunda)
	}
}

// ─── N3 ─────────────────────────────────────────────────────────────────────

func TestPagamentoManual_PedidoJaPagoEhRecusado(t *testing.T) {
	gw := gatewayPronto()
	gw.cartStatus = "paid"
	svc := newWebhookService(gw)

	err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual)
	if err == nil {
		t.Fatal("aceitou confirmar pagamento manual sobre um pedido já pago — o " +
			"dedup do gateway não bate com o sintético, então o fan-out rodaria " +
			"de novo sobre uma venda fechada")
	}
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 409 {
		t.Errorf("erro = %v; esperava 409", err)
	}
	if len(gw.updateCalls) != 0 {
		t.Errorf("marcou pagamento mesmo assim: %v", gw.updateCalls)
	}
	if len(gw.emitOrder) != 0 {
		t.Errorf("emitiu fato mesmo assim: %v", gw.emitOrder)
	}
}

func TestPagamentoManual_PedidoEstornadoEhRecusado(t *testing.T) {
	gw := gatewayPronto()
	gw.cartStatus = "refunded"
	svc := newWebhookService(gw)

	if err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual); err == nil {
		t.Error("aceitou marcar como pago um pedido estornado")
	}
	if len(gw.emitOrder) != 0 {
		t.Errorf("emitiu fato sobre pedido estornado: %v", gw.emitOrder)
	}
}

// Carrinho que não existe mais: 404, e nada é escrito.
func TestPagamentoManual_CarrinhoInexistente(t *testing.T) {
	gw := gatewayPronto()
	gw.cartStatus = ""
	svc := newWebhookService(gw)

	err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual)
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != 404 {
		t.Errorf("erro = %v; esperava 404", err)
	}
}

// ─── N4 ─────────────────────────────────────────────────────────────────────

// Sem itens não há o que separar. A recusa vem ANTES da escrita: marcar pago e
// só depois descobrir que não há item deixaria uma venda paga sem pedido.
func TestPagamentoManual_PedidoSemItensNaoEhMarcadoPago(t *testing.T) {
	gw := gatewayPronto()
	gw.gmv = 0
	svc := newWebhookService(gw)

	if err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual); err == nil {
		t.Fatal("aceitou marcar como pago um pedido sem itens")
	}
	if len(gw.updateCalls) != 0 {
		t.Errorf("marcou pago antes de checar os itens: %v", gw.updateCalls)
	}
	if len(gw.emitOrder) != 0 {
		t.Errorf("emitiu fato para pedido vazio: %v", gw.emitOrder)
	}
}

// ─── N5 ─────────────────────────────────────────────────────────────────────

// Cancelado pela loja e o dinheiro entrou assim mesmo: o dinheiro manda. Mesma
// inversão que o webhook do gateway já faz.
func TestPagamentoManual_CarrinhoCanceladoPelaLojaEhRestaurado(t *testing.T) {
	gw := gatewayPronto()
	gw.updateErr = payment.ErrCartNotPayable
	gw.restored = true
	svc := newWebhookService(gw)

	if err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual); err != nil {
		t.Fatalf("ConfirmManualPayment: %v", err)
	}
	if gw.restoreCalls != 1 {
		t.Errorf("restaurações = %d, esperava 1", gw.restoreCalls)
	}
	if len(gw.emitOrder) != 1 {
		t.Errorf("após restaurar, o fato precisa sair como num pagamento normal: %v", gw.emitOrder)
	}
}

// Expirado (ou cancelado de um jeito que a restauração não cobre): recusa com
// instrução. Marcar pago às escondidas deixaria o pedido pago com o prazo
// vencido, e a compradora sem link.
func TestPagamentoManual_CarrinhoExpiradoRecusaComInstrucao(t *testing.T) {
	gw := gatewayPronto()
	gw.updateErr = payment.ErrCartNotPayable
	gw.restored = false
	svc := newWebhookService(gw)

	err := svc.ConfirmManualPayment(context.Background(), cartManual, storeManual)
	if err == nil {
		t.Fatal("aceitou confirmar pagamento em carrinho expirado")
	}
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) {
		t.Fatalf("erro sem tipo: %v", err)
	}
	if svcErr.Reason != string(httpx.CodeCartExpired) {
		t.Errorf("reason = %q, esperava %q — é por ele que a tela oferece regerar o link",
			svcErr.Reason, httpx.CodeCartExpired)
	}
	if len(gw.emitOrder) != 0 {
		t.Errorf("emitiu fato para carrinho expirado: %v", gw.emitOrder)
	}
}
