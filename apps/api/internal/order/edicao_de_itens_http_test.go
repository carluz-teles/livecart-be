package order_test

// As três rotas de edição de item, pela app Fiber de verdade.
//
// Os testes de serviço cobrem a fronteira de posse; este cobre a camada que
// costuma quebrar sem ninguém notar: a rota registrada no caminho certo (com
// `:itemId` depois de `:id`, num grupo que já tem `/:id` — ordem errada faz o
// PATCH de item cair no PATCH de pedido), o corpo sendo lido, o 422 carregando a
// chave do campo, e a resposta trazendo o PEDIDO INTEIRO relido em vez do item.
//
// O editor é falso de propósito: o real é o checkout, que fala com o Tiny. O que
// se mede aqui é o caminho HTTP, não a mecânica de estoque.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/httpx"
)

func appDePedidos(t *testing.T, storeID string, editor order.CartItemEditor) (*fiber.App, *editorFalso) {
	t.Helper()

	svc := order.NewService(order.NewRepository(testPool), zap.NewNop())
	falso, _ := editor.(*editorFalso)
	if editor != nil {
		svc.SetCartItemEditor(editor)
	}

	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("store_id", storeID)
		return c.Next()
	})
	order.NewHandler(svc, nil).RegisterRoutes(app)
	return app, falso
}

func chamar(t *testing.T, app *fiber.App, method, path, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

// ─── caminho completo ───────────────────────────────────────────────────────

func TestEdicaoDeItensHTTP_AsTresRotasRespondemOPedidoInteiro(t *testing.T) {
	requireDB(t)

	storeID, eventID := seedIsolatedStore(t, "http-edit1")
	prod := seedProduct(t, storeID, 8990)
	cartID := insertCart(t, eventID, "@cliente", "tok-httpedit1", 92001, "pending", nil)
	addItem(t, cartID, prod, 2, 8990)

	app, editor := appDePedidos(t, storeID, &editorFalso{})

	casos := []struct {
		nome   string
		method string
		path   string
		body   string
		op     string
	}{
		{
			nome:   "POST adiciona produto",
			method: http.MethodPost,
			path:   "/orders/" + cartID + "/items",
			body:   `{"productId":"` + prod + `","quantity":2}`,
			op:     "add",
		},
		{
			nome:   "PATCH fixa a quantidade",
			method: http.MethodPatch,
			path:   "/orders/" + cartID + "/items/item-abc",
			body:   `{"quantity":5}`,
			op:     "set",
		},
		{
			nome:   "DELETE remove o item",
			method: http.MethodDelete,
			path:   "/orders/" + cartID + "/items/item-abc",
			op:     "remove",
		},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			status, body := chamar(t, app, tt.method, tt.path, tt.body)
			if status != http.StatusOK {
				t.Fatalf("status %d, esperava 200: %s", status, body)
			}

			// A resposta é o pedido relido: sem ela a tela teria de fazer uma
			// segunda requisição, que pode cruzar com outra edição.
			var envelope struct {
				Data struct {
					ID    string `json:"id"`
					Items []struct {
						ProductName string `json:"productName"`
						Quantity    int    `json:"quantity"`
					} `json:"items"`
					TotalAmount   int64 `json:"totalAmount"`
					PayableAmount int64 `json:"payableAmount"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("resposta não é o detalhe do pedido: %v (%s)", err, body)
			}
			if envelope.Data.ID != cartID {
				t.Errorf("resposta trouxe o pedido %q, esperava %q", envelope.Data.ID, cartID)
			}
			if len(envelope.Data.Items) == 0 {
				t.Error("resposta veio sem itens — a tabela ficaria vazia depois de editar")
			}
			if envelope.Data.PayableAmount == 0 {
				t.Error("resposta veio com payableAmount 0 — o rodapé de valores zeraria")
			}
		})
	}

	if len(editor.chamadas) != 3 {
		t.Fatalf("o checkout recebeu %d chamada(s), esperava 3: %+v",
			len(editor.chamadas), editor.chamadas)
	}
	// A ordem de registro importa: `/:id/items/:itemId` num grupo que já tem
	// `/:id` só casa se estiver registrado corretamente — senão o PATCH de item
	// cai no PATCH do pedido e altera STATUS em vez de quantidade.
	esperado := []string{"add", "set", "remove"}
	for i, op := range esperado {
		if editor.chamadas[i].op != op {
			t.Errorf("chamada %d foi %q, esperava %q — rota casou no handler errado",
				i, editor.chamadas[i].op, op)
		}
	}
	if got := editor.chamadas[1]; got.itemID != "item-abc" || got.quantity != 5 {
		t.Errorf("PATCH chegou como %+v; o :itemId ou o corpo não foram lidos", got)
	}
}

// ─── recusas ────────────────────────────────────────────────────────────────

func TestEdicaoDeItensHTTP_CorpoInvalidoRecusaComCampo(t *testing.T) {
	requireDB(t)

	storeID, eventID := seedIsolatedStore(t, "http-edit2")
	prod := seedProduct(t, storeID, 1000)
	cartID := insertCart(t, eventID, "@cliente", "tok-httpedit2", 92002, "pending", nil)
	addItem(t, cartID, prod, 1, 1000)

	app, editor := appDePedidos(t, storeID, &editorFalso{})

	casos := []struct {
		nome   string
		method string
		path   string
		body   string
		campo  string
	}{
		{
			nome:   "adicionar sem produto",
			method: http.MethodPost,
			path:   "/orders/" + cartID + "/items",
			body:   `{"quantity":1}`,
			campo:  "productId",
		},
		{
			nome:   "adicionar com produto que não é uuid",
			method: http.MethodPost,
			path:   "/orders/" + cartID + "/items",
			body:   `{"productId":"vestido","quantity":1}`,
			campo:  "productId",
		},
		{
			// O piso mole do ozzo: sem Required, quantity 0 passaria.
			nome:   "quantidade zero ao adicionar",
			method: http.MethodPost,
			path:   "/orders/" + cartID + "/items",
			body:   `{"productId":"` + prod + `","quantity":0}`,
			campo:  "quantity",
		},
		{
			nome:   "quantidade zero no patch não é remover",
			method: http.MethodPatch,
			path:   "/orders/" + cartID + "/items/item-1",
			body:   `{"quantity":0}`,
			campo:  "quantity",
		},
		{
			nome:   "quantidade absurda",
			method: http.MethodPatch,
			path:   "/orders/" + cartID + "/items/item-1",
			body:   `{"quantity":999999}`,
			campo:  "quantity",
		},
	}

	for _, tt := range casos {
		t.Run(tt.nome, func(t *testing.T) {
			status, body := chamar(t, app, tt.method, tt.path, tt.body)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, esperava 422: %s", status, body)
			}
			var envelope struct {
				Fields map[string]string `json:"fields"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("erro ilegível: %v (%s)", err, body)
			}
			if _, ok := envelope.Fields[tt.campo]; !ok {
				t.Errorf("erro não aponta o campo %q: %s", tt.campo, body)
			}
		})
	}

	if len(editor.chamadas) != 0 {
		t.Errorf("corpo inválido chegou ao checkout: %+v", editor.chamadas)
	}
}

// Pedido de outra loja: 404 pela rota, e o checkout não é chamado. Repetido no
// nível HTTP porque é aqui que o storeID vem do contexto — um handler que lesse
// a loja do corpo ou da query furaria o escopo sem o serviço notar.
func TestEdicaoDeItensHTTP_PedidoDeOutraLojaNaoEditavel(t *testing.T) {
	requireDB(t)

	_, eventoDaOutra := seedIsolatedStore(t, "http-edit3b")
	lojaDaOutra := mustStoreOf(t, eventoDaOutra)
	prod := seedProduct(t, lojaDaOutra, 5000)
	cartDaOutra := insertCart(t, eventoDaOutra, "@outra", "tok-httpedit3b", 92003, "pending", nil)
	addItem(t, cartDaOutra, prod, 1, 5000)

	minhaLoja, _ := seedIsolatedStore(t, "http-edit3a")
	app, editor := appDePedidos(t, minhaLoja, &editorFalso{})

	status, body := chamar(t, app, http.MethodPost,
		"/orders/"+cartDaOutra+"/items",
		`{"productId":"`+prod+`","quantity":1}`)
	if status != http.StatusNotFound {
		t.Errorf("status %d, esperava 404: %s", status, body)
	}
	if len(editor.chamadas) != 0 {
		t.Errorf("o carrinho alheio chegou ao checkout: %+v", editor.chamadas)
	}
}
