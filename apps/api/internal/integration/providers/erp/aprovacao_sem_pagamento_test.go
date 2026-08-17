package erp

// Aprovar o pedido no Tiny e lançar o financeiro são coisas DIFERENTES, e
// estavam amarradas.
//
// `if order.Payment != nil { ApproveOrder(...) }` usava a presença do bloco
// financeiro como sinal de "esta venda está paga". Funcionava enquanto os dois
// andavam juntos — todo pagamento vinha do gateway e trazia parcelas, taxas e
// data de liberação.
//
// O pagamento recebido POR FORA quebra o par: o dinheiro entrou, então o pedido
// tem de sair Aprovado, mas o lançamento financeiro não é nosso para fazer — a
// forma como o dinheiro entrou só o lojista sabe, e ele registra no ERP. Sem
// separar as duas coisas, o pedido chegaria "Em aberto" e a lojista teria de
// aprovar um por um na mão. Foi exatamente o estado dos três pedidos que
// falharam na live de 16/08.
//
// O discriminador certo é a intenção da chamada, não a presença de um campo.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// tinySpyDePedido registra o que o Tiny recebeu em cada rota do pós-criação.
type tinySpyDePedido struct {
	mu         sync.Mutex
	situacoes  []int
	pagamentos int // POSTs de pedido que levaram bloco `pagamento`
}

func (s *tinySpyDePedido) servidor(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "/situacao"):
			var corpo struct {
				Situacao int `json:"situacao"`
			}
			_ = json.NewDecoder(r.Body).Decode(&corpo)
			s.situacoes = append(s.situacoes, corpo.Situacao)
			w.WriteHeader(http.StatusNoContent)

		case strings.Contains(r.URL.Path, "/marcadores"),
			strings.Contains(r.URL.Path, "/lancar-estoque"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pedidos"):
			var corpo map[string]any
			_ = json.NewDecoder(r.Body).Decode(&corpo)
			if _, tem := corpo["pagamento"]; tem {
				s.pagamentos++
			}
			_, _ = w.Write([]byte(`{"id":900,"numeroPedido":"26999"}`))

		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func pedidoBase() ERPOrder {
	return ERPOrder{
		ExternalID:  "cart-manual",
		ContactID:   "807484994",
		TotalAmount: 17980,
		Items: []ERPOrderItem{
			{ProductID: "845175101", Quantity: 2, UnitPrice: 8990},
		},
	}
}

// O caso que motivou a mudança: venda paga por fora. Aprova, e NÃO manda
// financeiro.
func TestPedidoPagoPorForaSaiAprovadoSemFinanceiro(t *testing.T) {
	spy := &tinySpyDePedido{}
	srv := spy.servidor(t)
	defer srv.Close()

	pedido := pedidoBase()
	pedido.Approve = true
	pedido.Payment = nil // o lojista lança o recebimento no próprio ERP

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedido); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if len(spy.situacoes) != 1 || spy.situacoes[0] != 3 {
		t.Errorf("situações enviadas = %v; esperava um único 3 (Aprovado). Sem isso "+
			"o pedido chega \"Em aberto\" e a lojista aprova na mão, um por um",
			spy.situacoes)
	}
	if spy.pagamentos != 0 {
		t.Errorf("foi junto um bloco de pagamento (%d) — o recebimento é lançado "+
			"pelo lojista no ERP, com a forma que só ele sabe", spy.pagamentos)
	}
}

// A outra metade: a conversão PRÉ-pagamento continua sem aprovar. Ela cria o
// pedido para segurar a grade, e a venda ainda não aconteceu — aprovar ali
// registraria como vendido o que ninguém pagou.
func TestConversaoAntesDoPagamentoNaoAprova(t *testing.T) {
	spy := &tinySpyDePedido{}
	srv := spy.servidor(t)
	defer srv.Close()

	pedido := pedidoBase()
	pedido.Approve = false
	pedido.Payment = nil

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedido); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if len(spy.situacoes) != 0 {
		t.Errorf("conversão pré-pagamento mudou a situação para %v — a venda ainda "+
			"não aconteceu", spy.situacoes)
	}
}

// Pagamento pelo gateway: aprova E manda o financeiro. É o comportamento de
// sempre, e separar as duas decisões não pode tê-lo mudado.
func TestPagamentoPeloGatewayAprovaEMandaFinanceiro(t *testing.T) {
	spy := &tinySpyDePedido{}
	srv := spy.servidor(t)
	defer srv.Close()

	pedido := pedidoBase()
	pedido.Approve = true
	pedido.Payment = &providers.ERPOrderPayment{
		Method:    "pix",
		PaymentID: "ch_abc",
		Amount:    17980,
	}

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedido); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	if len(spy.situacoes) != 1 || spy.situacoes[0] != 3 {
		t.Errorf("situações = %v; o pagamento pelo gateway continua aprovando", spy.situacoes)
	}
	if spy.pagamentos != 1 {
		t.Errorf("bloco de pagamento enviado %d vez(es); o gateway continua "+
			"lançando o financeiro", spy.pagamentos)
	}
}
