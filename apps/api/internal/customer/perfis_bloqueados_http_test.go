package customer

// Fluxo completo da tela de Perfis bloqueados, pela rota HTTP de verdade.
//
// Os outros testes cobrem as pontas (a query contra o banco, o ozzo contra o
// DTO). Este cobre o meio, que é onde o fluxo costuma arrebentar sem ninguém
// notar: rota registrada no grupo certo, query string parseada, storeID vindo do
// contexto, envelope e nomes de campo em camelCase que o front consome, e o 422
// carregando a chave do campo.
//
// O storeID entra por c.Locals("store_id"), exatamente como o middleware de auth
// faz — é o que permite exercitar a rota sem Clerk.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// appDePerfis monta a app com a MESMA rota e o MESMO ErrorHandler da produção.
func appDePerfis(t *testing.T, storeID string) *fiber.App {
	t.Helper()

	svc := NewService(NewRepository(custTestQueries), zap.NewNop())
	h := NewHandler(svc)

	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("store_id", storeID)
		return c.Next()
	})
	h.RegisterRoutes(app)
	return app
}

type respostaDaBusca struct {
	Data []struct {
		Handle            string  `json:"handle"`
		MessageCount      int     `json:"messageCount"`
		OrderMessageCount int     `json:"orderMessageCount"`
		LastSeenAt        *string `json:"lastSeenAt"`
		Blocked           bool    `json:"blocked"`
	} `json:"data"`
}

func chamarBusca(t *testing.T, app *fiber.App, querystring string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/customers/handles?"+querystring, nil)
	res, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

func TestPerfisBloqueadosHTTP_FluxoCompleto(t *testing.T) {
	requireCustDB(t)
	s := seedLojaComLive(t, "http1")

	// A conta de instrução da loja: fala e gera pedido indevido.
	comentar(t, s.eventID, "cantodaart.oficial", "manda 1042 2", "added_to_cart")
	comentar(t, s.eventID, "cantodaart.oficial", "é assim que pede", "no_intent")
	// Compradora de verdade, com arroba parecido — não pode ser bloqueada por engano.
	comentar(t, s.eventID, "cantodaart_fan", "quero 1042", "added_to_cart")

	app := appDePerfis(t, s.storeID)

	// ─── 1. Busca por trecho acha as duas ────────────────────────────────────
	status, body := chamarBusca(t, app, "search=cantodaart")
	if status != http.StatusOK {
		t.Fatalf("busca por trecho: status %d, body %s", status, body)
	}
	var busca respostaDaBusca
	if err := json.Unmarshal(body, &busca); err != nil {
		t.Fatalf("resposta não é o envelope esperado: %v (%s)", err, body)
	}
	if len(busca.Data) != 2 {
		t.Fatalf("busca por trecho devolveu %d perfil(is), esperava 2: %s", len(busca.Data), body)
	}

	// Os nomes de campo são contrato com o front: em snake_case a tela mostraria
	// zero mensagem para todo mundo, sem erro nenhum.
	porHandle := map[string]int{}
	for _, p := range busca.Data {
		porHandle[p.Handle] = p.OrderMessageCount
		if p.MessageCount == 0 {
			t.Errorf("perfil %s veio com messageCount=0 — campo não chegou ao front", p.Handle)
		}
		if p.LastSeenAt == nil {
			t.Errorf("perfil %s veio sem lastSeenAt", p.Handle)
		}
		if p.Blocked {
			t.Errorf("perfil %s veio bloqueado antes de qualquer bloqueio", p.Handle)
		}
	}
	if porHandle["cantodaart.oficial"] != 1 {
		t.Errorf("orderMessageCount da conta de instrução = %d, esperava 1",
			porHandle["cantodaart.oficial"])
	}

	// ─── 2. Modo exato isola a conta certa ──────────────────────────────────
	status, body = chamarBusca(t, app, "search=cantodaart.oficial&exact=true")
	if status != http.StatusOK {
		t.Fatalf("busca exata: status %d, body %s", status, body)
	}
	busca = respostaDaBusca{}
	_ = json.Unmarshal(body, &busca)
	if len(busca.Data) != 1 || busca.Data[0].Handle != "cantodaart.oficial" {
		t.Fatalf("busca exata devolveu %s; deveria isolar a conta pedida", body)
	}

	// ─── 3. Arroba com @ e maiúscula é a MESMA busca ─────────────────────────
	status, body = chamarBusca(t, app, "search=%40CantoDaArt.Oficial&exact=true")
	if status != http.StatusOK {
		t.Fatalf("busca com @ e maiúscula: status %d, body %s", status, body)
	}
	busca = respostaDaBusca{}
	_ = json.Unmarshal(body, &busca)
	if len(busca.Data) != 1 {
		t.Errorf("arroba colado com @ e caixa alta não achou o perfil: %s — a "+
			"lojista digita do jeito que lembra", body)
	}

	// ─── 4. Bloquear pela API e a busca refletir ────────────────────────────
	bloquear(t, app, "cantodaart.oficial")

	status, body = chamarBusca(t, app, "search=cantodaart")
	if status != http.StatusOK {
		t.Fatalf("busca após bloqueio: status %d, body %s", status, body)
	}
	busca = respostaDaBusca{}
	_ = json.Unmarshal(body, &busca)
	for _, p := range busca.Data {
		switch p.Handle {
		case "cantodaart.oficial":
			if !p.Blocked {
				t.Error("perfil bloqueado não veio marcado na busca — a tela ofereceria " +
					"bloquear de novo quem já está bloqueado")
			}
		case "cantodaart_fan":
			if p.Blocked {
				t.Error("bloquear a conta de instrução marcou a compradora de arroba " +
					"parecido — é o pior erro possível nesta tela")
			}
		}
	}
}

// bloquear chama o endpoint real de bloqueio.
func bloquear(t *testing.T, app *fiber.App, handle string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/customers/blocks",
		strings.NewReader(fmt.Sprintf(`{"handle":%q}`, handle)))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("POST /blocks: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST /blocks: status %d, body %s", res.StatusCode, body)
	}
}

// ─── recusas ────────────────────────────────────────────────────────────────

// O termo curto é recusado com a CHAVE do campo. Sem a chave o front mostra um
// toast solto em vez de marcar o input, e a lojista não sabe o que corrigir.
func TestPerfisBloqueadosHTTP_TermoCurtoRecusadoComCampo(t *testing.T) {
	requireCustDB(t)
	s := seedLojaComLive(t, "http2")
	app := appDePerfis(t, s.storeID)

	for _, qs := range []string{"search=a", "search=", "search=%40"} {
		status, body := chamarBusca(t, app, qs)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%q: status %d, esperava 422 (body %s)", qs, status, body)
			continue
		}
		var envelope struct {
			Fields map[string]string `json:"fields"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("%q: resposta de erro ilegível: %v (%s)", qs, err, body)
			continue
		}
		if _, ok := envelope.Fields["search"]; !ok {
			t.Errorf("%q: erro não aponta o campo `search`: %s", qs, body)
		}
	}
}

// Sem termo NÃO existe listagem: é a regra de produto que impede a tela de
// despejar a plateia inteira, onde escolher o perfil certo vira caça ao erro.
func TestPerfisBloqueadosHTTP_SemTermoNaoLista(t *testing.T) {
	requireCustDB(t)
	s := seedLojaComLive(t, "http3")
	comentar(t, s.eventID, "alguem", "oi", "no_intent")
	comentar(t, s.eventID, "outroalguem", "oi", "no_intent")

	app := appDePerfis(t, s.storeID)

	status, body := chamarBusca(t, app, "")
	if status == http.StatusOK {
		t.Fatalf("busca sem termo respondeu 200 — listaria a plateia inteira: %s", body)
	}
}

// A busca é escopada na loja do contexto. Um storeID trocado não pode enxergar
// a plateia de outra loja.
func TestPerfisBloqueadosHTTP_NaoAtravessaLojas(t *testing.T) {
	requireCustDB(t)
	minha := seedLojaComLive(t, "http4a")
	outra := seedLojaComLive(t, "http4b")

	comentar(t, outra.eventID, "perfil.da.outra", "1042 1", "added_to_cart")

	app := appDePerfis(t, minha.storeID)
	status, body := chamarBusca(t, app, "search=perfil.da.outra&exact=true")
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}
	var busca respostaDaBusca
	_ = json.Unmarshal(body, &busca)
	if len(busca.Data) != 0 {
		t.Errorf("busca vazou perfil de outra loja: %s", body)
	}
}

// Loja inválida no contexto vira 422, não 500.
func TestPerfisBloqueadosHTTP_LojaInvalida(t *testing.T) {
	requireCustDB(t)
	app := appDePerfis(t, "nao-e-uuid")

	status, body := chamarBusca(t, app, "search=fulano")
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status %d, esperava 422: %s", status, body)
	}
}
