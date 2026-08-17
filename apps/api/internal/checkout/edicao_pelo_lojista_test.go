package checkout

// Edição de itens pelo LOJISTA, no painel de pedidos.
//
// A mecânica é a mesma do checkout público — de propósito, é lá que vivem a
// trava otimista, o rollback do ERP, o cancelamento do PIX e a promoção da fila.
// O que muda são três decisões de política, e são elas que este arquivo trava:
//
//   M1 o toggle `cart_allow_edit` NÃO se aplica ao lojista. A coluna é
//      "allow customers to edit cart on checkout page": usá-la contra a dona da
//      loja travaria a correção exatamente nas lojas que desligaram a edição
//      pelo comprador — as que mais precisam corrigir pelo painel.
//   M2 os outros estados (pago, cancelado, expirado) continuam valendo para os
//      DOIS. Sem isso a edição pelo painel mexeria em estoque e reserva de ERP
//      de um pedido já pago ou já estornado.
//   M3 a auditoria distingue quem editou. `source` não é enfeite de log:
//      GetCheckoutUpsellMetricsByEvent conta como upsell o carrinho com mutação
//      `buyer_checkout`, então gravar a edição do lojista com o default faria o
//      painel reportar o aumento dele como compra espontânea da cliente.
//   M4 a whitelist da sessão não limita o lojista — foi o pedido explícito
//      ("adicionar outros produtos cadastrados no LiveCart").

import (
	"errors"
	"testing"
	"time"

	"livecart/apps/api/lib/httpx"
)

// ─── M1 + M2 ────────────────────────────────────────────────────────────────

func TestEdicaoPeloLojista_MatrizDeEstado(t *testing.T) {
	agora := time.Now()
	passado := agora.Add(-time.Hour)
	futuro := agora.Add(time.Hour)

	casos := []struct {
		nome       string
		cart       CartRow
		byMerchant bool
		recusa     bool
		porque     string
	}{
		{
			nome:       "loja com edição do comprador DESLIGADA: lojista edita",
			cart:       CartRow{Status: "checkout", PaymentStatus: "pending", AllowEdit: false, ExpiresAt: &futuro},
			byMerchant: true,
			recusa:     false,
			porque:     "é o caso que motivou a feature — a lojista precisa corrigir o pedido",
		},
		{
			nome:       "loja com edição do comprador DESLIGADA: comprador não edita",
			cart:       CartRow{Status: "checkout", PaymentStatus: "pending", AllowEdit: false, ExpiresAt: &futuro},
			byMerchant: false,
			recusa:     true,
			porque:     "afrouxar aqui daria ao comprador o que a loja desligou",
		},
		{
			nome:       "pedido PAGO: lojista também não edita",
			cart:       CartRow{Status: "checkout", PaymentStatus: "paid", AllowEdit: true, ExpiresAt: &futuro},
			byMerchant: true,
			recusa:     true,
			porque:     "mexeria em estoque e ERP de uma venda fechada",
		},
		{
			nome:       "pedido CANCELADO: lojista também não edita",
			cart:       CartRow{Status: "cancelled", PaymentStatus: "pending", AllowEdit: true, ExpiresAt: &futuro},
			byMerchant: true,
			recusa:     true,
			porque:     "o estorno já devolveu estoque e reserva",
		},
		{
			nome:       "pedido EXPIRADO por status: lojista também não edita",
			cart:       CartRow{Status: "expired", PaymentStatus: "pending", AllowEdit: true},
			byMerchant: true,
			recusa:     true,
			porque:     "carrinho morto",
		},
		{
			nome:       "prazo VENCIDO no relógio: lojista também não edita",
			cart:       CartRow{Status: "checkout", PaymentStatus: "pending", AllowEdit: true, ExpiresAt: &passado},
			byMerchant: true,
			recusa:     true,
			porque:     "o worker de expiração pode não ter passado ainda",
		},
		{
			nome:       "sem prazo e não pago: os dois editam",
			cart:       CartRow{Status: "active", PaymentStatus: "pending", AllowEdit: true},
			byMerchant: false,
			recusa:     false,
			porque:     "carrinho de evento aberto não tem prazo correndo",
		},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			err := assertCartMutable(&tt.cart, tt.byMerchant, agora)
			if tt.recusa && err == nil {
				t.Fatalf("deveria recusar (%s), aceitou", tt.porque)
			}
			if !tt.recusa && err != nil {
				t.Fatalf("deveria aceitar (%s), recusou: %v", tt.porque, err)
			}
		})
	}
}

// O prazo vencido tem de sair como CART_EXPIRED, não como um 409 genérico: é o
// código pelo qual a tela oferece regerar o link em vez de dizer "tente de novo".
func TestEdicaoPeloLojista_PrazoVencidoTemCodigoDeExpirado(t *testing.T) {
	agora := time.Now()
	vencido := agora.Add(-time.Minute)
	cart := &CartRow{Status: "checkout", PaymentStatus: "pending", AllowEdit: true, ExpiresAt: &vencido}

	err := assertCartMutable(cart, true, agora)
	if err == nil {
		t.Fatal("prazo vencido foi aceito")
	}
	var svcErr *httpx.ServiceError
	if !errors.As(err, &svcErr) {
		t.Fatalf("erro não é ServiceError (%T) — a tela não teria código para ramificar", err)
	}
	if svcErr.Code != 422 {
		t.Errorf("status = %d, esperava 422", svcErr.Code)
	}
	if svcErr.Reason != string(httpx.CodeCartExpired) {
		t.Errorf("reason = %q, esperava %q — é por ele que a tela oferece regerar o link",
			svcErr.Reason, httpx.CodeCartExpired)
	}
}

// ─── M3 ─────────────────────────────────────────────────────────────────────

func TestEdicaoPeloLojista_AuditoriaDistingueQuemEditou(t *testing.T) {
	if got := mutationSource(true); got != "merchant" {
		t.Errorf("edição do lojista auditada como %q — entraria nas métricas de "+
			"upsell como se a cliente tivesse aumentado o carrinho sozinha", got)
	}
	if got := mutationSource(false); got != "buyer_checkout" {
		t.Errorf("edição do comprador auditada como %q, esperava buyer_checkout", got)
	}
}

// O valor tem de ser um dos aceitos pelo CHECK de cart_mutations.source, senão a
// gravação estoura e a mutação fica sem auditoria (o log é best-effort).
func TestEdicaoPeloLojista_SourceCabeNoCheckDaTabela(t *testing.T) {
	permitidos := map[string]bool{"buyer_checkout": true, "live_add": true, "merchant": true}
	for _, byMerchant := range []bool{true, false} {
		if got := mutationSource(byMerchant); !permitidos[got] {
			t.Errorf("source %q não está no CHECK da coluna", got)
		}
	}
}

// ─── M4 ─────────────────────────────────────────────────────────────────────

func TestEdicaoPeloLojista_WhitelistDaSessaoNaoLimitaOPainel(t *testing.T) {
	foraDaLive := &EventProductConfig{IsAllowed: false}
	naLive := &EventProductConfig{IsAllowed: true}

	if !productAllowedForCart(foraDaLive, true) {
		t.Error("lojista foi barrado de somar produto fora da whitelist da sessão — " +
			"era o pedido explícito, adicionar outros produtos do catálogo")
	}
	if productAllowedForCart(foraDaLive, false) {
		t.Error("comprador conseguiu pedir produto fora da whitelist da sessão; a " +
			"whitelist existe exatamente para limitar isso durante a live")
	}
	if !productAllowedForCart(naLive, false) {
		t.Error("produto liberado na sessão foi barrado para o comprador")
	}
}
