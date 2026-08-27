package erp

// Três pedidos pagos ficaram fora de sincronia na live de 16/08, todos com o
// mesmo desfecho: `create order failed: status 409: Esse registro já existe`,
// três retentativas e dead letter — enquanto o pedido estava no Tiny o tempo
// todo.
//
// A sequência que produz isso: o handler tem orçamento de tempo, o POST /pedidos
// chega ao Tiny e cria o pedido, e a RESPOSTA não volta antes do prazo. Do nosso
// lado nada foi criado; do lado do Tiny, foi. As tentativas seguintes recebem
// 409, que é a verdade — o registro existe — mas era lido como falha.
//
// Pior: a aprovação (`PUT /pedidos/{id}/situacao`) roda DEPOIS do POST voltar.
// Sem a resposta, ela nunca acontece, e o pedido fica "Em aberto" em vez de
// "Aprovado". Foi exatamente o que o lojista viu no painel do Tiny: dois em
// aberto, e um aprovado (aquele cuja resposta chegou a tempo).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// tinyFake grava as rotas chamadas para que o teste afirme a SEQUÊNCIA, não só
// o resultado.
type tinyFake struct {
	rotas           []string
	pedidosPost     int
	responderPedido func(tentativa int, w http.ResponseWriter)
	// ancoraAchaID: o id que a busca por marcador devolve. Vazio = não existe
	// pedido com aquela âncora.
	ancoraAchaID string
	// corpoDoPedido guarda o corpo do POST /pedidos, para o teste afirmar que a
	// âncora viajou junto.
	corpoDoPedido string
}

func (f *tinyFake) servidor(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.rotas = append(f.rotas, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/pedidos") && r.URL.Query().Get("marcadores") != "":
			if f.ancoraAchaID == "" {
				_, _ = w.Write([]byte(`{"itens":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"itens":[{"id":` + f.ancoraAchaID + `}]}`))

		case r.Method == http.MethodPost && r.URL.Path == "/pedidos":
			f.pedidosPost++
			if b, err := io.ReadAll(r.Body); err == nil {
				f.corpoDoPedido = string(b)
			}
			f.responderPedido(f.pedidosPost, w)

		default: // marcadores, situacao
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func (f *tinyFake) chamou(rota string) bool {
	for _, r := range f.rotas {
		if r == rota {
			return true
		}
	}
	return false
}

// pedidoPago é a venda fechada pelo gateway: aprova E lança o financeiro. A
// aprovação passou a viajar em `Approve` — antes era inferida da presença do
// bloco de pagamento, e essa inferência quebrava o pagamento recebido por fora,
// que fecha a venda sem trazer financeiro.
func pedidoPago() ERPOrder {
	return ERPOrder{
		Approve:     true,
		ExternalID:  "c1ec50cc-940b-46d6-bf41-d1336d9f9d35",
		ContactID:   "809820176",
		TotalAmount: 12650,
		Items:       []ERPOrderItem{{ProductID: "846772972", Quantity: 1, UnitPrice: 12650}},
		Payment:     &providers.ERPOrderPayment{Method: "pix", PaidAt: time.Now(), Amount: 12650},
	}
}

func TestPedidoCriadoRecebeAAncoraDeBusca(t *testing.T) {
	f := &tinyFake{responderPedido: func(_ int, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":847673655,"numeroPedido":"26954"}`))
	}}
	srv := f.servidor(t)
	defer srv.Close()

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedidoPago()); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// O carimbo é a ÚNICA âncora buscável. O `numeroOrdemCompra` viaja no corpo e
	// parece dispensá-lo, mas `GET /pedidos?numeroOrdemCompra=` é ignorado em
	// silêncio pela API: devolve 200 e a conta inteira. Medido em 26/08/2026 —
	// uma busca por âncora inexistente trouxe os 92 pedidos da conta, e o
	// primeiro deles parecia um resultado legítimo.
	if !f.chamou("POST /pedidos/847673655/marcadores") {
		t.Errorf("o pedido criado não recebeu o marcador; sem ele uma retomada não "+
			"o reencontra. rotas: %v", f.rotas)
	}
	if !strings.Contains(f.corpoDoPedido, "numeroOrdemCompra") {
		t.Errorf("o POST /pedidos não levou numeroOrdemCompra — ele não serve de "+
			"filtro, mas é o que deixa o vínculo legível na tela do lojista. corpo: %s",
			f.corpoDoPedido)
	}
}

func TestConflitoAdotaOPedidoQueJaExiste(t *testing.T) {
	f := &tinyFake{
		ancoraAchaID: "847673655",
		responderPedido: func(_ int, w http.ResponseWriter) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"mensagem":"Esse registro já existe"}`))
		},
	}
	srv := f.servidor(t)
	defer srv.Close()

	res, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedidoPago())
	if err != nil {
		t.Fatalf("409 continuou sendo falha — o pedido existe no Tiny e a venda fica "+
			"fora de sincronia mesmo estando lá: %v", err)
	}
	if res == nil || res.OrderID != "847673655" {
		t.Fatalf("adoção devolveu %+v, esperava o pedido encontrado pelo marcador", res)
	}

	// O passo que faltava nos pedidos "Em aberto": a aprovação roda depois do
	// POST voltar, e não voltou. Adotar sem aprovar deixaria o defeito de pé.
	if !f.chamou("PUT /pedidos/847673655/situacao") {
		t.Errorf("o pedido adotado não foi aprovado — ficaria 'Em aberto' no Tiny; rotas: %v", f.rotas)
	}
}

func TestConflitoSemMarcadorContinuaSendoFalha(t *testing.T) {
	// Pedidos criados ANTES de o carimbo existir não são localizáveis. Inventar
	// sucesso aqui seria pior que falhar: o carrinho ficaria marcado como
	// concluído apontando para um pedido que ninguém sabe qual é.
	f := &tinyFake{
		ancoraAchaID: "",
		responderPedido: func(_ int, w http.ResponseWriter) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"mensagem":"Esse registro já existe"}`))
		},
	}
	srv := f.servidor(t)
	defer srv.Close()

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedidoPago()); err == nil {
		t.Fatal("409 sem pedido localizável virou sucesso — o carrinho fecharia apontando para o nada")
	}
}

func TestOrdemDosPassosNaCriacao(t *testing.T) {
	f := &tinyFake{responderPedido: func(_ int, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"id":900,"numeroPedido":"1"}`))
	}}
	srv := f.servidor(t)
	defer srv.Close()

	if _, err := newTinyAgainst(t, srv).CreateOrder(context.Background(), pedidoPago()); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// A ordem importa e é o que explica o "Em aberto": criar → marcar → aprovar.
	// Se o marcador viesse depois da aprovação, um timeout entre os dois deixaria
	// o pedido aprovado porém invisível para a retomada.
	iMarcador, iAprova := -1, -1
	for i, r := range f.rotas {
		switch r {
		case "POST /pedidos/900/marcadores":
			iMarcador = i
		case "PUT /pedidos/900/situacao":
			iAprova = i
		}
	}
	if iMarcador == -1 || iAprova == -1 {
		t.Fatalf("marcador ou aprovação não aconteceram; rotas: %v", f.rotas)
	}
	if iMarcador > iAprova {
		t.Errorf("o marcador foi gravado depois da aprovação; um timeout entre os "+
			"dois deixa o pedido sem âncora de busca. rotas: %v", f.rotas)
	}
}
