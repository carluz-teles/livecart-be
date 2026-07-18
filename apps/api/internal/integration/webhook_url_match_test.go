package integration

import "testing"

// Regressão do bug de campo (primeiro cliente, 18/07/2026): o lojista colou a
// URL do webhook com barra no final. A Pagar.me entregou TODOS os eventos no
// endpoint certo — os logs registraram "pagarme webhook live-test event
// received" — mas a comparação de string exata acusava "URL diferente da
// nossa", mandando o lojista consertar algo que já estava correto.
func TestSameWebhookURL(t *testing.T) {
	const expected = "https://api.livecart.com.br/api/webhooks/pagarme/f280a988-f8e9-4392-9112-8d26f97a580c"

	iguais := []struct {
		nome     string
		entregue string
	}{
		{"idêntica", expected},
		{"barra no final (o caso do cliente)", expected + "/"},
		{"várias barras no final", expected + "///"},
		{"espaços ao redor", "  " + expected + "  "},
		{"host em maiúsculas", "https://API.LiveCart.com.BR/api/webhooks/pagarme/f280a988-f8e9-4392-9112-8d26f97a580c"},
		{"esquema em maiúsculas", "HTTPS://api.livecart.com.br/api/webhooks/pagarme/f280a988-f8e9-4392-9112-8d26f97a580c"},
		{"query string extra", expected + "?source=pagarme"},
		{"barra + query", expected + "/?x=1"},
	}
	for _, tc := range iguais {
		t.Run("igual/"+tc.nome, func(t *testing.T) {
			if !sameWebhookURL(tc.entregue, expected) {
				t.Fatalf("deveria ser considerada a MESMA URL: %q vs %q", tc.entregue, expected)
			}
		})
	}

	diferentes := []struct {
		nome     string
		entregue string
	}{
		{"outro domínio", "https://api.outraloja.com.br/api/webhooks/pagarme/f280a988-f8e9-4392-9112-8d26f97a580c"},
		{"outra loja (store id trocado)", "https://api.livecart.com.br/api/webhooks/pagarme/00000000-0000-0000-0000-000000000000"},
		{"outro caminho", "https://api.livecart.com.br/api/webhooks/mercado_pago/f280a988-f8e9-4392-9112-8d26f97a580c"},
		{"sem o store id", "https://api.livecart.com.br/api/webhooks/pagarme"},
		{"vazia", ""},
	}
	for _, tc := range diferentes {
		t.Run("diferente/"+tc.nome, func(t *testing.T) {
			if sameWebhookURL(tc.entregue, expected) {
				t.Fatalf("deveria ser considerada URL DIFERENTE: %q vs %q", tc.entregue, expected)
			}
		})
	}
}
