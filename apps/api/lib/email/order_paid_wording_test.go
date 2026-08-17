package email

// O e-mail de pagamento confirmado dizia, no título, "Pedido #X a caminho" —
// logo abaixo da linha "✓ Pagamento confirmado", contradizendo a si mesmo. O
// pagamento tinha sido aprovado; nada havia sido despachado.
//
// Prometer envio no momento do pagamento gera a pior sequência possível para a
// loja: a compradora espera a entrega a partir daquele dia, cobra antes do
// prazo real e desconfia da loja quando o pedido "sumiu". O assunto e a versão
// em texto puro sempre estiveram certos; só o HTML mentia.

import (
	"strings"
	"testing"
)

// Frases que afirmam DESPACHO. Nenhuma pode aparecer no e-mail de pagamento
// confirmado — ali o pedido ainda nem foi separado.
var frasesDeEnvio = []string{
	"a caminho",
	"foi despachado",
	"foi enviado",
	"saiu para entrega",
}

func TestEmailDePagamentoNaoPrometeEnvio(t *testing.T) {
	corpo := strings.ToLower(orderPaidShellHTML)

	for _, frase := range frasesDeEnvio {
		if strings.Contains(corpo, frase) {
			t.Errorf("o e-mail de pagamento confirmado contém %q — ele afirma despacho "+
				"num momento em que só houve aprovação de pagamento", frase)
		}
	}
}

// O contrário também importa: o e-mail de ENVIO precisa dizer que foi enviado.
// Sem esta metade, "corrigir" o texto do pagamento apagando a frase dos dois
// passaria neste arquivo e deixaria a compradora sem aviso de despacho.
func TestEmailDeEnvioAnunciaODespacho(t *testing.T) {
	corpo := strings.ToLower(orderShippedShellHTML)

	encontrou := false
	for _, frase := range frasesDeEnvio {
		if strings.Contains(corpo, frase) {
			encontrou = true
			break
		}
	}
	if !encontrou {
		t.Errorf("o e-mail de envio não anuncia o despacho; esperava uma de %v", frasesDeEnvio)
	}
}

// O botão só serve se levar à página pública com o token. Um link sem o
// `{{.TrackingURL}}` cairia numa rota que exige login de LOJISTA — a compradora
// não tem conta, e foi exatamente esse o defeito relatado.
func TestBotaoDoPedidoUsaAURLDeRastreio(t *testing.T) {
	for nome, corpo := range map[string]string{
		"pagamento confirmado": orderPaidShellHTML,
		"pedido enviado":       orderShippedShellHTML,
	} {
		if !strings.Contains(corpo, "{{.TrackingURL}}") {
			t.Errorf("e-mail de %s não usa {{.TrackingURL}} no link — a compradora "+
				"não tem como abrir o pedido sem o token", nome)
		}
	}
}
