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
	// ancoraAchaID: o id que a varredura dos pedidos recentes devolve. Vazio =
	// não existe
	// pedido com aquela âncora.
	ancoraAchaID string
	// ancoraDoCarrinho é o cart id que a âncora do pedido encontrado carrega.
	// Tem de bater com o do pedido sendo criado, senão a varredura o descarta —
	// que é justamente a proteção contra adotar o pedido de outro comprador.
	ancoraDoCarrinho string
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
		// A adoção não busca mais por marcador: varre os pedidos recentes e
		// COMPARA a âncora. O fake responde a listagem já com o campo, que é o
		// caminho barato (sem releitura por pedido).
		case r.Method == http.MethodGet && r.URL.Path == "/pedidos" && r.URL.Query().Get("limit") != "":
			if f.ancoraAchaID == "" {
				_, _ = w.Write([]byte(`{"itens":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"itens":[{"id":` + f.ancoraAchaID +
				`,"numeroOrdemCompra":"lc-cart-` + f.ancoraDoCarrinho + `"}]}`))

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

	// O MARCADOR NÃO É ESCRITO, e isto trava essa decisão. Os marcadores do
	// Tiny são a etiqueta de organização do LOJISTA; enchê-los de metadado
	// nosso polui a ferramenta de trabalho dele para resolver um problema
	// nosso. A âncora vive em numeroOrdemCompra e nas observações.
	if f.chamou("POST /pedidos/847673655/marcadores") {
		t.Errorf("escreveu marcador no pedido — os marcadores são do lojista. "+
			"rotas: %v", f.rotas)
	}
	if !strings.Contains(f.corpoDoPedido, "numeroOrdemCompra") {
		t.Errorf("o POST /pedidos não levou numeroOrdemCompra — ele não serve de "+
			"filtro, mas é a âncora que a adoção compara ao varrer os pedidos "+
			"recentes. corpo: %s", f.corpoDoPedido)
	}
}

func TestConflitoAdotaOPedidoQueJaExiste(t *testing.T) {
	f := &tinyFake{
		ancoraAchaID:     "847673655",
		ancoraDoCarrinho: "c1ec50cc-940b-46d6-bf41-d1336d9f9d35",
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
		t.Fatalf("adoção devolveu %+v, esperava o pedido encontrado pela âncora", res)
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
	// A âncora agora viaja DENTRO do POST /pedidos, então ela não pode chegar
	// tarde — é gravada com o pedido ou não é gravada. O que resta verificar é
	// que a aprovação aconteceu e que nenhum marcador foi escrito.
	iAprova := -1
	for i, r := range f.rotas {
		if r == "PUT /pedidos/900/situacao" {
			iAprova = i
		}
		if strings.HasSuffix(r, "/marcadores") {
			t.Errorf("escreveu marcador — os marcadores são do lojista. rotas: %v", f.rotas)
		}
	}
	if iAprova == -1 {
		t.Fatalf("a aprovação não aconteceu; rotas: %v", f.rotas)
	}
}
