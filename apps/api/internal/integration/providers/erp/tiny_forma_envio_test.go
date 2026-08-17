package erp

// Um pedido pago que não chega ao ERP é o pior desfecho possível: o dinheiro
// entrou, o carrinho fechou, o estoque foi baixado e o lojista não tem pedido
// para separar. Foi o que aconteceu em produção em 16/08/26 — uma venda de
// R$ 4,90 paga por Pix morreu com:
//
//	transportador.formaEnvio.id: Forma de envio não habilitada
//
// A forma de envio depende de cadastro dentro do Tiny, e a listagem de
// /formas-envio devolve só id, nome e tipo — não existe campo dizendo se está
// habilitada. Não há como escolher certo na consulta; só o POST revela. Por
// isso a recusa desse campo não pode derrubar o pedido.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func pedidoDeTeste(ship *providers.ERPOrderShipping) ERPOrder {
	return ERPOrder{
		ExternalID:  "cart-abc",
		ContactID:   "807484994",
		TotalAmount: 490,
		Items: []ERPOrderItem{
			{ProductID: "845175101", Quantity: 1, UnitPrice: 490},
		},
		Shipping: ship,
	}
}

const recusaDaFormaDeEnvio = `{"mensagem":"Ocorreram erros de validação","detalhes":[{"campo":"transportador.formaEnvio.id","mensagem":"Forma de envio não habilitada"}]}`

// captura registra os POSTs de pedido que chegaram ao Tiny.
type capturaDePedidos struct {
	payloads    []map[string]any
	formasEnvio int
}

func (c *capturaDePedidos) servidor(t *testing.T, responder func(tentativa int, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rotas auxiliares do pós-criação (marcador, situação, estoque) não são
		// tentativas de criar pedido — contá-las mascararia o que o teste mede.
		if strings.Contains(r.URL.Path, "/marcadores") ||
			strings.Contains(r.URL.Path, "/situacao") ||
			strings.Contains(r.URL.Path, "/lancar-estoque") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.Contains(r.URL.Path, "formas-envio") {
			c.formasEnvio++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"itens":[{"id":842150825,"nome":"SmartEnvios"}]}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.payloads = append(c.payloads, body)
		w.Header().Set("Content-Type", "application/json")
		responder(len(c.payloads), w)
	}))
}

func formaEnvioDoPayload(payload map[string]any) (any, bool) {
	transportador, ok := payload["transportador"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := transportador["formaEnvio"]
	return v, ok
}

func TestPedidoEntraNoTinyMesmoComFormaDeEnvioRecusada(t *testing.T) {
	cap := &capturaDePedidos{}
	srv := cap.servidor(t, func(tentativa int, w http.ResponseWriter) {
		if tentativa == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(recusaDaFormaDeEnvio))
			return
		}
		_, _ = w.Write([]byte(`{"id":123,"numeroPedido":"456","situacao":"aberto"}`))
	})
	defer srv.Close()

	res, err := newTinyAgainst(t, srv).CreateOrder(context.Background(),
		pedidoDeTeste(&providers.ERPOrderShipping{Carrier: "Correios", Service: "PAC", CostCents: 1500}))
	if err != nil {
		t.Fatalf("o pedido pago não foi registrado no ERP: %v", err)
	}
	if res == nil || res.OrderID != "123" {
		t.Fatalf("resultado inesperado: %+v", res)
	}

	if len(cap.payloads) != 2 {
		t.Fatalf("esperava 2 tentativas (a original e a sem forma de envio), houve %d", len(cap.payloads))
	}
	if _, tem := formaEnvioDoPayload(cap.payloads[0]); !tem {
		t.Error("a primeira tentativa deveria levar a forma de envio — é o caminho normal")
	}
	if v, tem := formaEnvioDoPayload(cap.payloads[1]); tem {
		t.Errorf("o reenvio manteve a forma de envio (%v); seria recusado de novo pelo mesmo motivo", v)
	}

	// O lojista precisa saber por que o pedido entrou sem transportadora.
	obs, _ := cap.payloads[1]["observacoesInternas"].(string)
	if !strings.Contains(strings.ToLower(obs), "forma de envio") {
		t.Errorf("observacoesInternas do reenvio não avisa sobre a forma de envio: %q", obs)
	}
}

// O reenvio só faz sentido quando o problema é a forma de envio. Repetir um
// POST que vai ser recusado de novo gasta a janela de rate limit do lojista e
// atrasa o erro real.
func TestRecusaPorOutroMotivoNaoEhReenviada(t *testing.T) {
	cap := &capturaDePedidos{}
	srv := cap.servidor(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"mensagem":"Ocorreram erros de validação","detalhes":[{"campo":"itens[0].produto.id","mensagem":"Produto não encontrado"}]}`))
	})
	defer srv.Close()

	_, err := newTinyAgainst(t, srv).CreateOrder(context.Background(),
		pedidoDeTeste(&providers.ERPOrderShipping{Carrier: "Correios", CostCents: 1500}))
	if err == nil {
		t.Fatal("um produto inexistente não pode virar pedido criado")
	}
	if len(cap.payloads) != 1 {
		t.Errorf("houve %d tentativas; uma recusa não relacionada à forma de envio não deve ser reenviada", len(cap.payloads))
	}
}

// Retirada na loja não é remessa. Antes, o carrier "Retirada na loja" não batia
// com nada em /formas-envio e caía no fallback para o agregador — carimbando
// uma entrega por SmartEnvios num pedido que o cliente vai buscar no balcão.
func TestRetiradaNaLojaNaoVinculaTransportadora(t *testing.T) {
	cap := &capturaDePedidos{}
	srv := cap.servidor(t, func(_ int, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":123,"numeroPedido":"456","situacao":"aberto"}`))
	})
	defer srv.Close()

	_, err := newTinyAgainst(t, srv).CreateOrder(context.Background(),
		pedidoDeTeste(&providers.ERPOrderShipping{Carrier: providers.StorePickupCarrier, Service: "Retirar na loja"}))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if len(cap.payloads) != 1 {
		t.Fatalf("esperava 1 tentativa, houve %d", len(cap.payloads))
	}
	if v, tem := formaEnvioDoPayload(cap.payloads[0]); tem {
		t.Errorf("retirada na loja saiu vinculada à forma de envio %v", v)
	}
	if cap.formasEnvio != 0 {
		t.Errorf("houve %d consulta(s) a /formas-envio para uma retirada na loja — não há transportadora a resolver", cap.formasEnvio)
	}
}

func TestDropFormaEnvioPreservaOResto(t *testing.T) {
	payload := map[string]any{
		"valorFrete": 15.0,
		"transportador": map[string]any{
			"fretePorConta": "D",
			"formaEnvio":    map[string]any{"id": int64(842150825)},
		},
		"observacoesInternas": "Transportadora: Correios",
	}

	if !dropFormaEnvio(payload) {
		t.Fatal("dropFormaEnvio devolveu false com a forma de envio presente")
	}

	transportador := payload["transportador"].(map[string]any)
	if _, tem := transportador["formaEnvio"]; tem {
		t.Error("formaEnvio continua no payload")
	}
	// fretePorConta e valorFrete não têm relação com o campo recusado: quem
	// paga o frete e quanto custa continuam valendo.
	if transportador["fretePorConta"] != "D" {
		t.Error("fretePorConta foi perdido junto")
	}
	if payload["valorFrete"] != 15.0 {
		t.Error("valorFrete foi perdido junto")
	}
	if obs, _ := payload["observacoesInternas"].(string); !strings.Contains(obs, "Transportadora: Correios") {
		t.Errorf("a observação original foi sobrescrita: %q", obs)
	}

	// Sem forma de envio no payload não há o que remover — e sem isso o
	// CreateOrder reenviaria um POST idêntico ao que acabou de ser recusado.
	if dropFormaEnvio(payload) {
		t.Error("dropFormaEnvio devolveu true numa segunda chamada, sem nada a remover")
	}
}
