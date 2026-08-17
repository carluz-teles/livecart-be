package erp

// Retirada na loja gerava um aviso falso em todo pedido.
//
// A decisão vivia em dois lugares: um `if` lá em cima pulava a consulta de
// transportadora, e um `switch` mais abaixo escolhia a mensagem. Sem id e sem
// erro, a retirada desembocava no `default` e saía como WARN dizendo
// "formaEnvio lookup returned no match" — afirmando uma consulta que nunca
// aconteceu, num caso em que não consultar é justamente o comportamento certo.
//
// Custo: todo pedido de retirada acendia um aviso, e quem fosse investigar
// procuraria um problema de cadastro de transportadora que não existe.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/integration/providers"
)

func tinyComLogObservado(t *testing.T, srv *httptest.Server) (*Tiny, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	tiny := newTinyAgainst(t, srv)
	tiny.Logger = zap.New(core)
	return tiny, logs
}

func servidorQueAceitaPedido(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "formas-envio") {
			_, _ = w.Write([]byte(`{"itens":[{"id":842150825,"nome":"SmartEnvios"}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/pedidos" {
			_, _ = w.Write([]byte(`{"id":900,"numeroPedido":"1"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func TestRetiradaNaLojaNaoGeraAvisoDeBuscaSemResultado(t *testing.T) {
	srv := servidorQueAceitaPedido(t)
	defer srv.Close()

	tiny, logs := tinyComLogObservado(t, srv)

	_, err := tiny.CreateOrder(context.Background(), ERPOrder{
		ExternalID:  "cart-retirada",
		ContactID:   "1",
		TotalAmount: 1000,
		Items:       []ERPOrderItem{{ProductID: "1", Quantity: 1, UnitPrice: 1000}},
		Shipping:    &providers.ERPOrderShipping{Carrier: providers.StorePickupCarrier},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	for _, entrada := range logs.All() {
		if strings.Contains(entrada.Message, "lookup returned no match") {
			t.Errorf("retirada gerou %q em nível %s — afirma uma consulta que não houve",
				entrada.Message, entrada.Level)
		}
		if entrada.Level == zapcore.WarnLevel {
			t.Errorf("retirada gerou WARN %q; não vincular transportadora é o comportamento correto",
				entrada.Message)
		}
	}

	if logs.FilterMessageSnippet("retirada na loja").Len() == 0 {
		t.Error("nenhum log explica por que o pedido saiu sem forma de envio")
	}
}

// A outra metade: quando a consulta ACONTECE e não acha nada, o aviso tem de
// continuar existindo. Silenciar os dois casos junto esconderia cadastro de
// transportadora faltando, que é problema real do lojista.
func TestBuscaSemResultadoContinuaAvisando(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "formas-envio") {
			_, _ = w.Write([]byte(`{"itens":[]}`)) // nada cadastrado
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/pedidos" {
			_, _ = w.Write([]byte(`{"id":900,"numeroPedido":"1"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tiny, logs := tinyComLogObservado(t, srv)

	_, err := tiny.CreateOrder(context.Background(), ERPOrder{
		ExternalID:  "cart-correios",
		ContactID:   "1",
		TotalAmount: 1000,
		Items:       []ERPOrderItem{{ProductID: "1", Quantity: 1, UnitPrice: 1000}},
		Shipping:    &providers.ERPOrderShipping{Carrier: "Correios"},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if logs.FilterMessageSnippet("lookup returned no match").Len() == 0 {
		t.Error("transportadora não cadastrada deixou de avisar — é problema real " +
			"do lojista e some do log junto com o falso positivo")
	}
}
