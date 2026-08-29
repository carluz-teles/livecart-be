package integration

// O simulador não pode existir fora de staging.
//
// Este é o teste que importa deste arquivo inteiro: o simulador cria comentário
// do nada, e comentário vira carrinho, que vira pedido no ERP de um lojista
// real. Se a porta abrir em produção, o estrago não é de teste.

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

func comAmbiente(t *testing.T, env string) {
	t.Helper()
	anterior, tinha := os.LookupEnv("APP_ENV")
	t.Cleanup(func() {
		if tinha {
			_ = os.Setenv("APP_ENV", anterior)
		} else {
			_ = os.Unsetenv("APP_ENV")
		}
	})
	if err := os.Setenv("APP_ENV", env); err != nil {
		t.Fatalf("setando APP_ENV: %v", err)
	}
}

// Fora de staging as rotas NÃO SÃO MONTADAS.
//
// O teste usa um app NU, sem a autenticação do grupo /stores, e por isso vê o
// 404 puro. No servidor real a autenticação roda antes do casamento de rota e
// devolve 401 — medido em APP_ENV=production, o mesmo 401 de uma rota
// inventada. O efeito de fora é melhor ainda: o simulador fica indistinguível
// de código que nunca foi escrito. O que este teste trava é a causa disso, que
// é o registro não acontecer.
func TestSimuladorNaoExisteForaDeStaging(t *testing.T) {
	for _, env := range []string{"production", "development", "test", ""} {
		nome := env
		if nome == "" {
			nome = "(vazio)"
		}
		t.Run(nome, func(t *testing.T) {
			comAmbiente(t, env)
			if config.IsStaging() {
				t.Fatalf("APP_ENV=%q não devia ser staging", env)
			}

			app := fiber.New()
			h := &WebhookHandler{}
			h.RegisterSimulatorRoutes(app.Group("/stores/:storeId"))

			for _, rota := range []struct{ metodo, caminho string }{
				{"POST", "/stores/x/simulador/live/midia"},
				{"POST", "/stores/x/simulador/live/comentario"},
				{"DELETE", "/stores/x/simulador/live/midia/abc"},
			} {
				req := httptest.NewRequest(rota.metodo, rota.caminho, nil)
				resp, err := app.Test(req)
				if err != nil {
					t.Fatalf("%s %s: %v", rota.metodo, rota.caminho, err)
				}
				if resp.StatusCode != fiber.StatusNotFound {
					t.Errorf("%s %s devolveu %d em APP_ENV=%q — quero 404: a rota "+
						"não pode nem existir fora de staging",
						rota.metodo, rota.caminho, resp.StatusCode, env)
				}
			}
		})
	}
}

// Em staging elas existem. Sem isto o teste acima passaria com um registro
// quebrado que nunca monta nada.
func TestSimuladorExisteEmStaging(t *testing.T) {
	comAmbiente(t, "staging")
	if !config.IsStaging() {
		t.Fatal("APP_ENV=staging devia ser staging")
	}

	app := fiber.New()
	h := &WebhookHandler{}
	h.RegisterSimulatorRoutes(app.Group("/stores/:storeId"))

	req := httptest.NewRequest("POST", "/stores/x/simulador/live/comentario", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("chamando: %v", err)
	}
	if resp.StatusCode == fiber.StatusNotFound {
		t.Error("a rota não foi montada em staging — o simulador não existiria")
	}
}

// A segunda camada, sozinha: mesmo que alguém mova a chamada de registro para
// fora do if, o handler recusa. Redundância de propósito.
func TestGuardaDoHandlerRecusaForaDeStaging(t *testing.T) {
	comAmbiente(t, "production")

	// Com o MESMO ErrorHandler da aplicação: o que se quer provar é o status
	// que o cliente recebe, e ele depende do mapeamento real do httpx.
	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	h := &WebhookHandler{}
	// Monta À FORÇA, sem passar por RegisterSimulatorRoutes — é a encenação do
	// erro humano que a redundância existe para conter.
	app.Post("/forcado", h.somenteStaging, func(c *fiber.Ctx) error {
		return c.SendString("NUNCA DEVIA CHEGAR AQUI")
	})

	resp, err := app.Test(httptest.NewRequest("POST", "/forcado", nil))
	if err != nil {
		t.Fatalf("chamando: %v", err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status %d, quero 403 — a guarda do handler é a rede que segura "+
			"um registro montado no lugar errado", resp.StatusCode)
	}
}

// O id derivado do @ é estável: o mesmo @ tem de cair sempre no mesmo
// comprador, senão cada comentário criaria um carrinho novo e o teste de
// carrinho acumulando — que é o mais importante — nunca aconteceria.
func TestMesmoArrobaGeraOMesmoComprador(t *testing.T) {
	a := idEstavelDoHandle("maria")
	b := idEstavelDoHandle("maria")
	c := idEstavelDoHandle("joana")
	if a != b {
		t.Errorf("o mesmo @ gerou ids diferentes (%s ≠ %s) — cada comentário "+
			"viraria um comprador novo e o carrinho nunca acumularia", a, b)
	}
	if a == c {
		t.Errorf("@ diferentes colidiram em %s", a)
	}
	if normalizarArroba(" @Maria ") != "maria" {
		t.Errorf("o @ não foi normalizado: %q", normalizarArroba(" @Maria "))
	}
	if idEstavelDoHandle(normalizarArroba("@MARIA")) != a {
		t.Error("maiúscula e arroba mudaram o comprador — o lojista digita dos dois jeitos")
	}
}

// O payload tem de ser o que a Meta manda, incluindo o carimbo em
// MILISSEGUNDOS: mandar segundos esconderia o bug de conversão que epochSeconds
// existe para tratar.
func TestPayloadSimuladoTemAFormaDaMeta(t *testing.T) {
	entry, change, corpo := montarComentarioIG("conta-1", "media-1", "c-1", "maria", "u-1", "quero 2")

	if change.Field != "live_comments" {
		t.Errorf("field = %q, quero live_comments", change.Field)
	}
	valor, ok := change.Value.(InstagramLiveCommentValue)
	if !ok {
		t.Fatalf("value não é InstagramLiveCommentValue: %T", change.Value)
	}
	if valor.Media.ID != "media-1" || valor.From.Username != "maria" || valor.Text != "quero 2" {
		t.Errorf("payload errado: %+v", valor)
	}
	if valor.Media.MediaProductType != "LIVE" {
		t.Errorf("mediaProductType = %q, quero LIVE", valor.Media.MediaProductType)
	}
	// 1e11 segundos é o ano 5138; 1e11 milissegundos é 1973. O carimbo de hoje
	// em ms passa desse limiar, em segundos não.
	if entry.Time < 1e11 {
		t.Errorf("entry.time = %d — a Meta manda MILISSEGUNDOS, e mandar segundos "+
			"aqui esconderia o bug de conversão", entry.Time)
	}
	if len(corpo) == 0 {
		t.Error("o corpo cru veio vazio — é a evidência que o processamento guarda")
	}
}
