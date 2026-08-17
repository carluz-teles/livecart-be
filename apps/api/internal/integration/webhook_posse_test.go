package integration

// A conta do gateway pode ser compartilhada com outra plataforma: a cantodaart
// usa a mesma conta Pagar.me no Shopify. O gateway entrega TODOS os eventos da
// conta para a URL cadastrada, então venda que não é nossa chega no nosso
// webhook.
//
// Sem checagem de posse o evento seguia para o banco e quebrava em
// "invalid UUID: ryoVyuBVr9nrtwNecjXgNwnYb", gastando as três retentativas até
// morrer na dead letter. Aconteceu duas vezes na madrugada de 17/08, com dois
// pedidos distintos da outra loja — quatro dead letters no total, sobre
// pagamentos que nunca foram nossos para processar.
//
// O discriminador é o `code`: gravamos sempre o UUID do carrinho, e o pedido do
// teste de webhook leva o prefixo LCWHTEST-.

import "testing"

func TestCodeDeOutraPlataformaNaoEhNosso(t *testing.T) {
	// Ids reais colhidos das dead letters de 17/08.
	forasteiros := []string{
		"ryoVyuBVr9nrtwNecjXgNwnYb",
		"r3QXW36sG0RkRTSKAuVLd3GWX",
	}

	for _, code := range forasteiros {
		if isLiveCartOrderCode(code) {
			t.Errorf("code %q foi aceito como nosso — é pedido de outra plataforma "+
				"na conta compartilhada e vira dead letter no processamento", code)
		}
	}
}

func TestCarrinhoNossoEhReconhecido(t *testing.T) {
	// Os três carrinhos da live de 16/08, formato que o checkout grava.
	nossos := []string{
		"c1ec50cc-940b-46d6-bf41-d1336d9f9d35",
		"f6f0be13-5a09-4193-b032-da300f8a3201",
		"38980178-408d-41c2-993b-58281efc36ed",
	}

	for _, code := range nossos {
		if !isLiveCartOrderCode(code) {
			t.Errorf("code %q foi recusado — é carrinho nosso e o pagamento seria "+
				"ignorado, deixando a venda sem baixa", code)
		}
	}
}

// O pedido descartável do teste de webhook também é nosso: recusá-lo faria o
// teste de entrega parar de funcionar, que é como o painel confirma que a URL
// está cadastrada.
func TestPedidoDoTesteDeWebhookContinuaSendoNosso(t *testing.T) {
	if !isLiveCartOrderCode(pagarmeWebhookTestOrderPrefix + "abc123") {
		t.Error("o pedido do teste de webhook foi tratado como de outra plataforma")
	}
}

// Code vazio NÃO é decidido aqui. Evento de charge nem sempre traz o campo, e
// tratá-lo como forasteiro descartaria pagamento legítimo — o discriminador
// definitivo roda depois, sobre a referência que a consulta ao gateway devolve.
func TestCodeVazioNaoEhDecididoPeloFormato(t *testing.T) {
	if isLiveCartOrderCode("") {
		t.Error("code vazio foi afirmado como nosso; a decisão dele não é por formato")
	}
}
