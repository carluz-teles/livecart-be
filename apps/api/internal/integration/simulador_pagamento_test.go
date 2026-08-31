package integration

// A CONTA DO SIMULADOR TEM DE SER A MESMA DO CHECKOUT.
//
// O simulador cobra um valor; se ele divergir do que o checkout cobraria, o
// lojista testa um número e a produção cobra outro — e o simulador vira uma
// fonte de confiança falsa, que é pior do que não ter simulador.
//
// A ordem das parcelas do desconto é o que mais importa: o desconto de PIX
// incide sobre (subtotal − cupom) e NUNCA sobre o frete. É a mesma regra de
// checkout/handler.go:479-486.

import (
	"strings"
	"testing"
)

func TestContaDoSimuladorSegueAOrdemDoCheckout(t *testing.T) {
	casos := []struct {
		nome                           string
		subtotal, cupom, frete         int64
		pixPercent                     int
		queroPixDesconto, queroCobrado int64
	}{
		{
			nome:     "sem desconto nenhum",
			subtotal: 10000, frete: 1500,
			queroCobrado: 11500,
		},
		{
			nome:     "só cupom",
			subtotal: 10000, cupom: 2000, frete: 1500,
			queroCobrado: 9500,
		},
		{
			// 10% de 10000 = 1000. O frete NÃO entra na base.
			nome:     "só PIX",
			subtotal: 10000, frete: 1500, pixPercent: 10,
			queroPixDesconto: 1000, queroCobrado: 10500,
		},
		{
			// A base do PIX é (10000 − 2000) = 8000; 10% = 800.
			// Se o PIX incidisse sobre o subtotal cheio daria 1000 — errado.
			nome:     "cupom E PIX: o PIX incide sobre o que sobrou",
			subtotal: 10000, cupom: 2000, frete: 1500, pixPercent: 10,
			queroPixDesconto: 800, queroCobrado: 8700,
		},
		{
			// Se o PIX incidisse sobre subtotal+frete daria 1150.
			nome:     "o frete nunca entra na base do PIX",
			subtotal: 10000, frete: 1500, pixPercent: 10,
			queroPixDesconto: 1000, queroCobrado: 10500,
		},
		{
			nome:     "cupom maior que o subtotal não deixa a conta negativa",
			subtotal: 5000, cupom: 9000, frete: 1000, pixPercent: 10,
			queroPixDesconto: 0, queroCobrado: 1000,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			base := c.subtotal - c.cupom
			if base < 0 {
				base = 0
			}
			pixDesconto := base * int64(c.pixPercent) / 100
			cobrado := base - pixDesconto + c.frete
			if cobrado < 0 {
				cobrado = 0
			}

			if pixDesconto != c.queroPixDesconto {
				t.Errorf("desconto PIX = %d, queria %d", pixDesconto, c.queroPixDesconto)
			}
			if cobrado != c.queroCobrado {
				t.Errorf("cobrado = %d, queria %d", cobrado, c.queroCobrado)
			}
		})
	}
}

// O simulador NÃO pode existir fora de staging. É a asserção que mais importa
// deste arquivo: forjar pagamento marca venda como paga, e uma rota dessas
// numa loja real é fraude com um POST.
func TestSimuladorDePagamentoSoExisteEmStaging(t *testing.T) {
	fonte := lerFonte(t, "simulador_pagamento.go")

	if !contemTodos(fonte,
		"func (h *WebhookHandler) RegisterPaymentSimulatorRoutes",
		"if !config.IsStaging() {",
		"return\n\t}",
	) {
		t.Error("o registro das rotas perdeu a guarda de staging — em produção elas passariam a existir")
	}
	if !contemTodos(fonte, "func (h *WebhookHandler) somenteStagingPagamento", "httpx.CodeStagingOnly") {
		t.Error("a segunda camada (middleware por handler) sumiu")
	}
}

// O simulador entra pela MESMA função do webhook real. Se alguém escrever um
// caminho paralelo aqui, o simulado deixa de provar qualquer coisa sobre o real.
func TestSimuladorEntraPeloCaminhoDoWebhookReal(t *testing.T) {
	fonte := lerFonte(t, "simulador_pagamento.go")
	if !contemTodos(fonte, "h.payment.AplicarStatusDePagamento(") {
		t.Error("o simulador deixou de chamar AplicarStatusDePagamento — a única costura " +
			"aceitável é o gateway; tudo depois dele tem de ser o caminho de produção")
	}
	// Só o CÓDIGO — o comentário do topo cita `cart.paid` justamente para
	// explicar que não é o simulador quem o emite, e uma catraca que lê
	// comentário reclamaria da documentação que ela existe para proteger.
	codigo := semComentarios(fonte)
	for _, proibido := range []string{
		"UpdateCartPaymentStatus(", "EmitEvent(", "cart.paid",
	} {
		if contemTodos(codigo, proibido) {
			t.Errorf("o simulador faz %q por conta própria — isso é caminho paralelo, "+
				"e a divergência com a produção aparece como bug de produção", proibido)
		}
	}
}

// contemTodos é o predicado das catracas de fonte deste arquivo. `lerFonte`
// já existe em busca_estrangulada_test.go e é reusado.
func contemTodos(s string, partes ...string) bool {
	for _, p := range partes {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// semComentarios devolve só as linhas de código. As catracas deste arquivo
// perguntam o que o simulador FAZ, e comentário não faz nada.
func semComentarios(fonte string) string {
	var out []string
	for _, linha := range strings.Split(fonte, "\n") {
		if corte := strings.TrimSpace(linha); strings.HasPrefix(corte, "//") {
			continue
		}
		out = append(out, linha)
	}
	return strings.Join(out, "\n")
}
