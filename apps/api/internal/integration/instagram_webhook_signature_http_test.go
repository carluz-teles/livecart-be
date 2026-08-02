package integration

// Prova no nível HTTP que o gate de assinatura decide o mesmo que a função pura:
// em modo observação nada é barrado, e com INSTAGRAM_WEBHOOK_ENFORCE_SIGNATURE
// ligado o payload sem assinatura válida recebe 401 antes de qualquer trabalho.
//
// O handler é construído com service nil de propósito: o caminho de rejeição
// retorna antes de tocar no service, e o caminho de aceite usa um payload com
// "entry": [] para não entrar no laço de processamento. Se alguém mover o check
// para depois do parse, estes testes quebram com panic — que é o sinal certo.

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

const emptyEntryPayload = `{"object":"instagram","entry":[]}`

func newWebhookTestApp(t *testing.T, secret, enforce string) *fiber.App {
	t.Helper()
	t.Setenv("INSTAGRAM_APP_SECRET", secret)
	t.Setenv("INSTAGRAM_WEBHOOK_ENFORCE_SIGNATURE", enforce)

	h := &WebhookHandler{logger: zap.NewNop()}
	app := fiber.New()
	app.Post("/api/webhooks/instagram", h.HandleInstagramWebhook)
	return app
}

func postWebhook(t *testing.T, app *fiber.App, body, sigHeader string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/webhooks/instagram", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sigHeader != "" {
		req.Header.Set("X-Hub-Signature-256", sigHeader)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestInstagramWebhookSignatureGate(t *testing.T) {
	const secret = "segredo-http-test"
	body := emptyEntryPayload
	valid := sign(t, []byte(body), secret)

	cases := []struct {
		name    string
		secret  string
		enforce string
		sig     string
		want    int
	}{
		// Deploy 1 — observação: nada é barrado, nem assinatura errada.
		{"observacao aceita sem assinatura", secret, "false", "", fiber.StatusOK},
		{"observacao aceita assinatura errada", secret, "false", sign(t, []byte("outro corpo"), secret), fiber.StatusOK},
		{"observacao aceita assinatura correta", secret, "false", valid, fiber.StatusOK},
		// Default: sem a env var, o comportamento tem de ser observação.
		{"sem a env var cai em observacao", secret, "", "", fiber.StatusOK},

		// Deploy 2 — aplicando.
		{"aplicando rejeita sem assinatura", secret, "true", "", fiber.StatusUnauthorized},
		{"aplicando rejeita assinatura errada", secret, "true", sign(t, []byte("outro corpo"), secret), fiber.StatusUnauthorized},
		{"aplicando rejeita header do emulador", secret, "true", "simulated", fiber.StatusUnauthorized},
		{"aplicando aceita assinatura correta", secret, "true", valid, fiber.StatusOK},

		// Sem segredo (dev/staging) a aplicação não pode derrubar a ingestão.
		{"aplicando sem segredo nao rejeita", "", "true", "", fiber.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newWebhookTestApp(t, tc.secret, tc.enforce)
			if got := postWebhook(t, app, body, tc.sig); got != tc.want {
				t.Errorf("status = %d, quero %d", got, tc.want)
			}
		})
	}
}

// O corpo vazio continua sendo 400 e essa checagem vem antes da assinatura —
// não faz sentido gastar HMAC em requisição sem corpo.
func TestInstagramWebhookEmptyBodyStillBadRequest(t *testing.T) {
	app := newWebhookTestApp(t, "segredo", "true")
	if got := postWebhook(t, app, "", ""); got != fiber.StatusBadRequest {
		t.Errorf("status = %d, quero %d", got, fiber.StatusBadRequest)
	}
}
