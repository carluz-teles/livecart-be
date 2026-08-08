package integration

// A URL do webhook precisa ATENDER a sondagem, não só o evento.
//
// O bug de campo: em 06/08, em produção, o painel da Tiny recusou o cadastro
// com "Não foi possível acessar a URL". A URL estava no ar e respondia 200 ao
// POST — medido de fora, pela internet, no mesmo minuto. O que ela não fazia
// era responder ao GET: registradas só como POST, as rotas devolviam 405 à
// verificação de alcance que o provedor faz ANTES de aceitar o cadastro.
//
// De fora, 405 e "fora do ar" são a mesma coisa: o provedor só quer saber se
// alguém atende naquele endereço.
//
// O Instagram nunca sofreu disso porque já tinha um GET, para o desafio de
// verificação da Meta — o que também explica por que o sintoma parecia
// específico da Tiny.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// providersComWebhook são os provedores que recebem evento por URL própria.
// Todo nome aqui tem de atender a sondagem, senão o lojista não consegue nem
// cadastrar a URL.
var providersComWebhook = []string{
	"mercado_pago", "pagarme", "tiny", "melhor_envio", "twilio",
}

// metodosDeSondagem é todo método que NÃO entrega evento. A lista é ampla
// porque não sabemos qual deles o validador de cada provedor usa — e um túnel
// apontando para um servidor de eco trivial, que responde 200 em qualquer
// método, foi aceito pela Tiny no mesmo minuto em que a nossa URL era recusada.
var metodosDeSondagem = []string{"GET", "HEAD", "OPTIONS", "PUT", "PATCH", "DELETE"}

func newProbeTestApp(t *testing.T) *fiber.App {
	t.Helper()
	h := &WebhookHandler{logger: zap.NewNop()}
	app := fiber.New()
	// slugResolver nil: a sondagem não passa pelo storeCtx, e é justamente isso
	// que o teste precisa provar — ela não pode depender de resolver loja
	// nenhuma para responder.
	h.RegisterRoutes(app, httpx.StoreSlugResolver(nil))
	return app
}

func TestSondagemDeWebhookRespondeOK(t *testing.T) {
	app := newProbeTestApp(t)

	for _, provider := range providersComWebhook {
		for _, method := range metodosDeSondagem {
			t.Run(provider+"/"+method, func(t *testing.T) {
				url := "/api/webhooks/" + provider + "/00000000-0000-0000-0000-000000000000"
				resp, err := app.Test(httptest.NewRequest(method, url, nil), 5000)
				if err != nil {
					t.Fatalf("%s %s: %v", method, url, err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != fiber.StatusOK {
					t.Errorf("%s %s = %d, quero 200 — com %d o provedor recusa o cadastro dizendo que a URL está inacessível",
						method, url, resp.StatusCode, resp.StatusCode)
				}
				// HEAD não carrega corpo, então não carrega content-type.
				if method == "HEAD" {
					return
				}
				if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
					t.Errorf("%s %s devolveu Content-Type %q, quero JSON — validador que faz parse da resposta engasga com texto puro",
						method, url, ct)
				}
			})
		}
	}
}

// A sondagem não pode virar porta de entrada: evento só entra por POST.
func TestSondagemNaoProcessaEvento(t *testing.T) {
	app := newProbeTestApp(t)

	// service é nil neste handler. Se a sondagem tocasse no fluxo de
	// processamento, este GET entraria em pânico em vez de responder 200 —
	// que é exatamente o sinal que se quer.
	resp, err := app.Test(
		httptest.NewRequest("GET", "/api/webhooks/tiny/00000000-0000-0000-0000-000000000000?tipo=estoque", nil),
		5000,
	)
	if err != nil {
		t.Fatalf("sondagem com query de evento: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("status = %d, quero 200", resp.StatusCode)
	}
}
