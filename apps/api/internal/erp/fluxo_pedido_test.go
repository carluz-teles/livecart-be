package erp

// O fluxo inteiro, da mensagem na live até a venda fechada.
//
// Cada teste aqui fixa uma regra que a medição contra o ERP real impôs, e o
// simulador (erpsimulado_test.go) reproduz o comportamento que a produziu. Ler
// os nomes em sequência é ler o fluxo.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp/erpwrite"
	"livecart/apps/api/internal/integration/providers"
)

func montar(saldos map[string]int) (*Service, *repoSimulado, *erpSimulado, *colabSimulado) {
	e := novoERPSimulado(saldos)
	r := novoRepoSimulado()
	c := &colabSimulado{erp: e, repo: r}
	s := NewService(r, c, zap.NewNop())
	s.SetOrderStatusRepository(r)
	s.SetWriteLimits(limitesAbertos())
	return s, r, e, c
}

// limitesAbertos desliga o estrangulamento nos testes de fluxo. O limitador é
// real em produção e tem os seus próprios testes; aqui ele só faria a suíte
// esperar a janela de 60s do balde sustentado para provar uma regra de negócio.
func limitesAbertos() erpwrite.Limits {
	return erpwrite.Limits{
		BurstN: 4096, BurstWindow: time.Millisecond,
		SustainedN: 1 << 20, SustWindow: time.Millisecond,
	}
}

func item(produtoID string, qtd int) NonWaitlistedCartItem {
	return NonWaitlistedCartItem{
		ID: "ci-" + produtoID, ProductID: produtoID, Quantity: qtd,
		UnitPrice: 2000, ProductName: "Produto " + produtoID,
		ProductExternalID: "ext-" + produtoID,
	}
}

// ─── 1. O primeiro comentário cria o pedido ─────────────────────────────────

// A regra que dá nome ao modelo: criar o pedido de venda JÁ segura a peça. O
// saldo físico não se mexe, `reservado` sobe, `disponivel` desce.
func TestPrimeiroComentarioCriaOPedidoESeguraAPeca(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("primeiro comentário: %v", err)
	}

	est := erp.estoque("ext-p1")
	if est.saldo != 5 {
		t.Errorf("saldo físico = %d, quero 5 INALTERADO — mexer nele é o que este "+
			"modelo existe para não fazer", est.saldo)
	}
	if est.reservado != 1 || est.disponivel() != 4 {
		t.Errorf("reservado=%d disponivel=%d, quero 1 e 4", est.reservado, est.disponivel())
	}
	if erp.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1", erp.criacoes)
	}
	if erp.lancamentos != 0 || erp.estornos != 0 {
		t.Errorf("lançamentos=%d estornos=%d, quero 0 e 0 — nenhum dos dois pertence "+
			"ao caminho da live", erp.lancamentos, erp.estornos)
	}
	if c := repo.carrinho("cart-1"); c.state != OrderStateOpen || c.externalOrderID == "" {
		t.Errorf("carrinho ficou em %q com pedido %q, quero 'open' com pedido gravado",
			c.state, c.externalOrderID)
	}
}

// ─── 2. O segundo comentário entra no MESMO pedido ──────────────────────────

func TestSegundoComentarioSomaNoMesmoPedido(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("1º comentário: %v", err)
	}
	// O comprador comenta de novo: o carrinho passa a ter 3 no total.
	repo.definirItens("cart-1", item("p1", 3))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria"); err != nil {
		t.Fatalf("2º comentário: %v", err)
	}

	if erp.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1 — o segundo comentário ENTRA no "+
			"pedido existente, não abre outro", erp.criacoes)
	}
	if erp.puts != 1 {
		t.Errorf("PUT /itens = %d, quero 1", erp.puts)
	}
	est := erp.estoque("ext-p1")
	if est.saldo != 5 || est.reservado != 3 || est.disponivel() != 2 {
		t.Errorf("saldo=%d reservado=%d disponivel=%d, quero 5/3/2 — a medição real "+
			"de dois comentários", est.saldo, est.reservado, est.disponivel())
	}
	ped := erp.pedido(repo.carrinho("cart-1").externalOrderID)
	if ped.itens["ext-p1"] != 3 {
		t.Errorf("grade do pedido = %d, quero 3", ped.itens["ext-p1"])
	}
}

// A grade é reconstruída do BANCO, nunca de um delta. É o que faz duas mutações
// concorrentes convergirem em vez de somarem duas vezes.
func TestMutacaoUsaAGradeDoBancoENaoODelta(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	// O banco diz 4. Mesmo chamando com um delta de 1, o pedido tem de ficar 4.
	repo.definirItens("cart-1", item("p1", 4))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 4 {
		t.Errorf("reservado = %d, quero 4 (a grade do banco)", got)
	}
}

// Remover o item do carrinho reduz a reserva pelo mesmo PUT.
func TestRemoverItemDevolveAPecaPeloMesmoPUT(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 10, "ext-p2": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3), item("p2", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")

	if got := erp.estoque("ext-p2").reservado; got != 2 {
		t.Fatalf("preparo: reservado de p2 = %d, quero 2", got)
	}
	// O comprador tira o p2 inteiro.
	repo.definirItens("cart-1", item("p1", 3))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := erp.estoque("ext-p2").reservado; got != 0 {
		t.Errorf("reservado de p2 = %d, quero 0 — o item saiu da grade", got)
	}
	if erp.estornos != 0 {
		t.Errorf("estornos = %d, quero 0 — devolver peça é PUT, nunca estorno", erp.estornos)
	}
}

// ─── 3. O pagamento não movimenta estoque ───────────────────────────────────

func TestPagamentoAprovaSemMovimentarEstoque(t *testing.T) {
	svc, repo, erp, colab := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	antes := erp.estoque("ext-p1")

	pago := time.Now()
	status := &providers.PaymentStatus{PaymentMethod: "pix", PaymentID: "pay-1", Installments: 1, PaidAt: &pago}
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", status); err != nil {
		t.Fatalf("confirmando pagamento: %v", err)
	}

	depois := erp.estoque("ext-p1")
	if depois != antes {
		t.Errorf("estoque mudou no pagamento: %+v → %+v. A reserva já estava de pé "+
			"desde o comentário; a baixa física é o faturamento do lojista", antes, depois)
	}
	if erp.lancamentos != 0 {
		t.Errorf("lançamentos = %d, quero 0 — o LiveCart não lança estoque", erp.lancamentos)
	}
	if erp.pagamentos != 1 || erp.situacoes != 1 {
		t.Errorf("parcelas=%d situações=%d, quero 1 e 1 (dois PUTs, e só)", erp.pagamentos, erp.situacoes)
	}
	ped := erp.pedido(repo.carrinho("cart-1").externalOrderID)
	if ped.situacao != providers.SituacaoAprovada {
		t.Errorf("situação = %d, quero 3 (Aprovada)", ped.situacao)
	}
	if repo.carrinho("cart-1").state != OrderStateConfirmed {
		t.Errorf("estado = %q, quero 'confirmed'", repo.carrinho("cart-1").state)
	}
	if colab.finalizados != 1 {
		t.Errorf("fatos de finalização = %d, quero 1", colab.finalizados)
	}
}

// Reentrega de webhook de pagamento não pode aprovar duas vezes.
func TestPagamentoReentregueEhIdempotente(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	for i := 0; i < 4; i++ {
		if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
			t.Fatalf("confirmação %d: %v", i, err)
		}
	}
	if erp.situacoes != 1 {
		t.Errorf("transições de situação = %d, quero 1 — as três reentregas seguintes "+
			"encontram 'confirmed' e saem", erp.situacoes)
	}
}

// Carrinho pago SEM pedido (loja ligou o ERP no meio da live) ganha o pedido no
// pagamento. Tarde para segurar estoque, mas a venda tem de existir.
func TestCarrinhoPagoSemPedidoGanhaUmNoPagamento(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 2))

	if err := svc.ConfirmERPOrderPayment(context.Background(), "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando: %v", err)
	}
	if erp.criacoes != 1 {
		t.Fatalf("pedidos criados = %d, quero 1", erp.criacoes)
	}
	ped := erp.pedido(repo.carrinho("cart-1").externalOrderID)
	if ped == nil || ped.situacao != providers.SituacaoAprovada {
		t.Errorf("pedido não ficou aprovado: %+v", ped)
	}
}

// ─── 4. Cancelamento devolve a reserva, sem estorno ─────────────────────────

func TestCancelamentoDevolveAReservaComUmaChamada(t *testing.T) {
	svc, repo, erp, colab := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")

	if err := svc.CancelERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("cancelando: %v", err)
	}
	est := erp.estoque("ext-p1")
	if est.saldo != 5 || est.reservado != 0 || est.disponivel() != 5 {
		t.Errorf("saldo=%d reservado=%d disponivel=%d, quero 5/0/5", est.saldo, est.reservado, est.disponivel())
	}
	if erp.estornos != 0 {
		t.Errorf("estornos = %d, quero 0. Num pedido que só reservou, estornar INFLA "+
			"a reserva — foi medido indo de 5 para 7 para 9", erp.estornos)
	}
	if repo.carrinho("cart-1").state != OrderStateCancelled {
		t.Errorf("estado = %q, quero 'cancelled'", repo.carrinho("cart-1").state)
	}
	if colab.cancelados != 1 {
		t.Errorf("fatos de cancelamento = %d, quero 1", colab.cancelados)
	}
}

// A prova pela contrapositiva: se o cancelamento acompanhasse um estorno, o
// estoque ficaria PIOR do que antes. Este teste executa o erro de propósito para
// deixar registrado o tamanho dele.
func TestEstornarAntesDeCancelarInflaAReserva(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	orderID := repo.carrinho("cart-1").externalOrderID
	_ = erp.ReverseOrderStock(ctx, orderID)
	if got := erp.estoque("ext-p1").reservado; got != 4 {
		t.Fatalf("reservado = %d, quero 4 — o estorno indevido dobrou a reserva", got)
	}
	_ = erp.ReverseOrderStock(ctx, orderID)
	if got := erp.estoque("ext-p1").disponivel(); got != -1 {
		t.Fatalf("disponivel = %d, quero -1 — a segunda chamada leva o saldo abaixo "+
			"de zero", got)
	}
	// E o cancelamento conserta tudo sozinho, que é a outra metade da regra.
	if err := svc.CancelERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("cancelando: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 0 {
		t.Errorf("reservado após cancelar = %d, quero 0 — cancelar devolve tudo, "+
			"inclusive a inflação", got)
	}
}

func TestExpiracaoDeCarrinhoCancelaOPedido(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	if err := svc.OnCartExpired(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("expirando: %v", err)
	}
	if got := erp.estoque("ext-p1").disponivel(); got != 5 {
		t.Errorf("disponivel = %d, quero 5", got)
	}
	if erp.estornos != 0 {
		t.Errorf("estornos = %d, quero 0", erp.estornos)
	}
}

// Carrinho sem pedido nenhum expira em silêncio — não há nada preso no ERP.
func TestExpiracaoDeCarrinhoSemPedidoNaoFalaComOERP(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 2))

	if err := svc.OnCartExpired(context.Background(), "cart-1", "loja-1"); err != nil {
		t.Fatalf("expirando: %v", err)
	}
	if erp.situacoes != 0 || erp.criacoes != 0 {
		t.Errorf("falou com o ERP sem ter pedido: situações=%d criações=%d", erp.situacoes, erp.criacoes)
	}
}

// ─── 5. A ÚNICA situação que autoriza um estorno ────────────────────────────

// O lojista lança o estoque à mão no painel enquanto a live rola. O pedido trava
// para edição, e o estorno é o que o destrava — uma vez, e sem relançar depois.
func TestLancamentoManualTravaAEdicaoEOEstornoDestrava(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	// O LOJISTA lança pelo painel: o físico cai, a reserva vira baixa.
	if err := erp.LaunchOrderStock(ctx, orderID); err != nil {
		t.Fatalf("lançamento manual: %v", err)
	}
	if got := erp.estoque("ext-p1"); got.saldo != 8 || got.reservado != 0 {
		t.Fatalf("preparo: saldo=%d reservado=%d, quero 8/0", got.saldo, got.reservado)
	}

	// Chega mais um comentário.
	repo.definirItens("cart-1", item("p1", 5))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação após lançamento manual: %v", err)
	}

	if erp.estornos != 1 {
		t.Errorf("estornos = %d, quero exatamente 1 — o mínimo para destravar", erp.estornos)
	}
	if erp.lancamentos != 1 {
		t.Errorf("lançamentos = %d, quero 1 (só o do lojista) — relançar travaria o "+
			"próximo comentário de novo", erp.lancamentos)
	}
	est := erp.estoque("ext-p1")
	if est.saldo != 10 || est.reservado != 5 || est.disponivel() != 5 {
		t.Errorf("saldo=%d reservado=%d disponivel=%d, quero 10/5/5 — o estorno "+
			"desfez a baixa e o PUT reajustou a reserva", est.saldo, est.reservado, est.disponivel())
	}
	if repo.carrinho("cart-1").stockLaunched {
		t.Error("erp_stock_launched continuou verdadeiro depois do estorno")
	}
}

// Erro de PUT que NÃO é o bloqueio não pode virar estorno. É a regra que impede
// um 500 transitório de destruir a reserva.
func TestErroComumNoPUTNaoAutorizaEstorno(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	erp.falharPut = errors.New("500 Internal Server Error")
	repo.definirItens("cart-1", item("p1", 5))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err == nil {
		t.Fatal("mutação devia falhar")
	}
	if erp.estornos != 0 {
		t.Errorf("estornos = %d, quero 0 — só a recusa por 'estoque lançado' autoriza "+
			"estornar; qualquer outro erro seria um estorno especulativo, e esse infla "+
			"a reserva", erp.estornos)
	}
	if got := erp.estoque("ext-p1").reservado; got != 2 {
		t.Errorf("reservado = %d, quero 2 intacto", got)
	}
}

// ─── 6. Rastreamento da situação ────────────────────────────────────────────

func TestRastreamentoGravaCadaTransicaoUmaVezSo(t *testing.T) {
	svc, repo, _, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	trajeto := []providers.ERPOrderStatus{
		providers.ERPOrderStatusAberto,
		providers.ERPOrderStatusAprovado,
		providers.ERPOrderStatusFaturado,
		providers.ERPOrderStatusEnviado,
		providers.ERPOrderStatusEntregue,
	}
	for _, st := range trajeto {
		// Cada aviso chega TRÊS vezes: o ERP reentrega até dez vezes quando não
		// recebe 200, e reentrega não é transição.
		for i := 0; i < 3; i++ {
			if err := svc.ObserveOrderStatus(ctx, "loja-1", orderID, "34", st, StatusSourceWebhook, nil); err != nil {
				t.Fatalf("observando %s: %v", st, err)
			}
		}
	}

	if len(repo.statusEventos) != len(trajeto) {
		t.Errorf("gravou %d passagens, quero %d — as reentregas não podem virar "+
			"linha, senão 'quando foi despachado?' deixa de ter resposta",
			len(repo.statusEventos), len(trajeto))
	}
	if got := repo.carrinho("cart-1").statusERP; got != string(providers.ERPOrderStatusEntregue) {
		t.Errorf("situação atual = %q, quero 'entregue'", got)
	}
	if got := repo.carrinho("cart-1").numeroPedido; got != "34" {
		t.Errorf("número do pedido = %q, quero '34'", got)
	}
}

// Pedido que não é de nenhum carrinho nosso: guarda a passagem e segue. Devolver
// erro faria o webhook responder não-200, e vinte desses fazem o ERP apagar a URL.
func TestSituacaoDePedidoQueNaoEhNossoNaoViraErro(t *testing.T) {
	svc, _, _, _ := montar(nil)
	err := svc.ObserveOrderStatus(context.Background(), "loja-1", "ped-de-outro-canal", "99",
		providers.ERPOrderStatusFaturado, StatusSourceWebhook, nil)
	if err != nil {
		t.Errorf("erro para pedido desconhecido: %v", err)
	}
}

// A varredura pergunta a situação de quem parou de se mexer — o conserto de
// webhook perdido.
func TestVarreduraPerguntaASituacaoDeQuemParou(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID
	_ = svc.ObserveOrderStatus(ctx, "loja-1", orderID, "34", providers.ERPOrderStatusAprovado, StatusSourceWebhook, nil)

	// O lojista despacha no ERP e o webhook se perde.
	_ = erp.SetOrderSituacao(ctx, orderID, providers.SituacaoEnviada)

	svc.RunERPOrderStatusSweep(ctx, time.Hour, 100)

	if got := repo.carrinho("cart-1").statusERP; got != string(providers.ERPOrderStatusEnviado) {
		t.Errorf("situação = %q, quero 'enviado' — a varredura existe para fechar "+
			"exatamente essa diferença", got)
	}
	ultimo := repo.statusEventos[len(repo.statusEventos)-1]
	if ultimo.Source != StatusSourceSweep {
		t.Errorf("fonte = %q, quero 'sweep' — a fonte é o que denuncia webhook "+
			"que parou de chegar", ultimo.Source)
	}
}

// O mapa situação↔slug é bijetivo. Um furo aqui faz o rastreamento mentir.
func TestMapaDeSituacoesEhCompletoEBijetivo(t *testing.T) {
	// Medido pedido a pedido em 26/08/2026, passando um pedido de teste por
	// todas as dez situações e lendo o webhook que cada uma disparou.
	esperado := map[providers.ERPOrderStatus]int{
		providers.ERPOrderStatusDadosIncompletos: 8,
		providers.ERPOrderStatusAberto:           0,
		providers.ERPOrderStatusAprovado:         3,
		providers.ERPOrderStatusPreparandoEnvio:  4,
		providers.ERPOrderStatusFaturado:         1,
		providers.ERPOrderStatusProntoEnvio:      7,
		providers.ERPOrderStatusEnviado:          5,
		providers.ERPOrderStatusEntregue:         6,
		providers.ERPOrderStatusCancelado:        2,
		providers.ERPOrderStatusNaoEntregue:      9,
	}
	for slug, codigo := range esperado {
		if got, ok := providers.SituacaoFromERPOrderStatus(slug); !ok || got != codigo {
			t.Errorf("%q → %d (ok=%v), quero %d", slug, got, ok, codigo)
		}
		if got, ok := providers.ERPOrderStatusFromSituacao(codigo); !ok || got != slug {
			t.Errorf("%d → %q (ok=%v), quero %q", codigo, got, ok, slug)
		}
		if got, ok := providers.ParseERPOrderStatus(string(slug)); !ok || got != slug {
			t.Errorf("parse %q falhou", slug)
		}
	}
	if _, ok := providers.ERPOrderStatusFromSituacao(77); ok {
		t.Error("código desconhecido virou situação — inventar um nome é pior do que " +
			"admitir que não conhecemos aquela")
	}
	if _, ok := providers.ParseERPOrderStatus("situacao_do_futuro"); ok {
		t.Error("slug desconhecido foi aceito")
	}
}

func TestSituacoesTerminaisSaemDaVarredura(t *testing.T) {
	terminais := []providers.ERPOrderStatus{
		providers.ERPOrderStatusEntregue,
		providers.ERPOrderStatusCancelado,
		providers.ERPOrderStatusNaoEntregue,
	}
	for _, st := range terminais {
		if !st.Terminal() {
			t.Errorf("%q devia ser terminal", st)
		}
	}
	for _, st := range []providers.ERPOrderStatus{
		providers.ERPOrderStatusAberto, providers.ERPOrderStatusAprovado,
		providers.ERPOrderStatusFaturado, providers.ERPOrderStatusEnviado,
		providers.ERPOrderStatusProntoEnvio, providers.ERPOrderStatusPreparandoEnvio,
		providers.ERPOrderStatusDadosIncompletos,
	} {
		if st.Terminal() {
			t.Errorf("%q não é terminal — o pedido ainda se move a partir dela", st)
		}
	}
}

// ─── 7. Bordas ──────────────────────────────────────────────────────────────

func TestLojaSemERPNaoQuebraOComentario(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.semIntegracao = true
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Errorf("loja sem ERP devia ser no-op silencioso, veio: %v", err)
	}
	if erp.criacoes != 0 {
		t.Errorf("criou pedido sem integração ativa")
	}
}

func TestCarrinhoSemItemVinculadoNaoCriaPedido(t *testing.T) {
	svc, repo, erp, colab := montar(map[string]int{"ext-p1": 5})
	colab.semVinculo = true
	repo.criarCarrinho("cart-1", NonWaitlistedCartItem{ID: "ci-1", ProductID: "p1", Quantity: 1, UnitPrice: 2000})

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if erp.criacoes != 0 {
		t.Errorf("criou pedido para carrinho sem item vinculado ao ERP")
	}
}

// Falha na criação deixa o carrinho em 'converting' — nunca de volta em 'none'.
// Voltar reabriria o caminho e criaria um SEGUNDO pedido para o mesmo carrinho.
func TestFalhaNaCriacaoNaoRegridePara_none(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	erp.falharCriacao = errors.New("503 do ERP")
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err == nil {
		t.Fatal("criação devia falhar")
	}
	if got := repo.carrinho("cart-1").state; got != OrderStateConverting {
		t.Errorf("estado = %q, quero 'converting' — a chamada em voo pode ter sucedido "+
			"do lado do ERP, e voltar para 'none' duplicaria o pedido", got)
	}
}

// Retomada de um 'converting' órfão: encontra o pedido pelo marcador em vez de
// criar outro.
func TestRetomadaAdotaOPedidoPeloMarcador(t *testing.T) {
	svc, repo, erp, colab := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))

	// Simula: o pedido foi criado e marcado, mas o processo morreu antes de
	// gravar o external_order_id.
	_, _ = colab.CreateERPOrderForCart(ctx, erp, nil, "loja-1", "cart-1")
	orderID := repo.carrinho("cart-1").externalOrderID
	_ = erp.AddOrderMarker(ctx, orderID, erpOrderMarker("cart-1"))
	_ = repo.UpdateCartExternalOrderID(ctx, "cart-1", "")
	_, _ = repo.TransitionCartERPOrderState(ctx, "cart-1", OrderStateNone, OrderStateConverting)

	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando: %v", err)
	}
	if erp.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1 — a adoção pelo marcador existe "+
			"exatamente para não criar o segundo", erp.criacoes)
	}
	if repo.carrinho("cart-1").externalOrderID != orderID {
		t.Errorf("adotou %q, quero %q", repo.carrinho("cart-1").externalOrderID, orderID)
	}
}

// Grade vazia é aceita pela API do ERP, mas nunca é o que o comprador quer.
func TestGradeVaziaEhRecusadaAqui(t *testing.T) {
	svc, repo, _, _ := montar(map[string]int{"ext-p1": 5})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	repo.definirItens("cart-1")
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err == nil {
		t.Error("grade vazia devia ser erro — um pedido sem itens não segura nada")
	}
}

// O contador local é o portão, e ele responde ao comprador na hora.
func TestAumentoSemEstoqueLocalDevolve422(t *testing.T) {
	svc, repo, _, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 1))
	repo.estoque["p1"] = 0

	_, err := svc.AdjustStockReservationDelta(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria", StockOpQtyIncrease)
	if err == nil {
		t.Fatal("aumento sem estoque devia falhar")
	}
	if !contemCodigo(err, "estoque insuficiente") {
		t.Errorf("erro = %v, quero a recusa por estoque insuficiente", err)
	}
}

// Recusa do ERP na mutação DESFAZ o contador local: o comprador não pode receber
// um "ok" sobre peça que o ERP não separou.
func TestRecusaDoERPDesfazOContadorLocal(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	repo.estoque["p1"] = 10
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	erp.falharPut = errors.New("503 do ERP")
	repo.definirItens("cart-1", item("p1", 4))
	if _, err := svc.AdjustStockReservationDelta(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria", StockOpQtyIncrease); err == nil {
		t.Fatal("aumento devia falhar")
	}
	if got := repo.estoque["p1"]; got != 10 {
		t.Errorf("contador local = %d, quero 10 — o decremento tem de ser desfeito", got)
	}
}

func contemCodigo(err error, trecho string) bool {
	return err != nil && fmt.Sprint(err) != "" && contains(fmt.Sprint(err), trecho)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ─── 8. A compensação sobrevive ao prazo ────────────────────────────────────

// Prazo estourado no meio da mutação NÃO pode deixar o carrinho preso.
//
// Este é o defeito que a live simulada de 15 compradores expôs: o `defer` que
// devolve o carrinho para 'open' rodava no MESMO contexto que acabara de ser
// cancelado, então o UPDATE morria junto e o carrinho ficava em 'mutating' —
// estado em que nenhum comentário seguinte consegue entrar. Seis dos quinze
// carrinhos pararam ali, com itens no banco que nunca chegaram ao pedido.
//
// A compensação de uma operação não pode depender do contexto dela.
func TestPrazoEstouradoNaoDeixaOCarrinhoPreso(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	// O ERP demora mais do que o prazo do chamador.
	erp.antesDoPut = func() { time.Sleep(40 * time.Millisecond) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	repo.definirItens("cart-1", item("p1", 5))
	_ = svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")

	if st := repo.carrinho("cart-1").state; st != OrderStateOpen {
		t.Fatalf("carrinho ficou em %q depois de um prazo estourado — preso em "+
			"'mutating' ele nunca mais aceita um comentário", st)
	}

	// E continua editável: o próximo comentário entra normalmente.
	erp.antesDoPut = nil
	if err := svc.MutateERPOrderItems(context.Background(), "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação seguinte: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 5 {
		t.Errorf("reservado = %d, quero 5 — a grade do banco tinha de chegar ao pedido", got)
	}
}

// ─── 9. Convergência: nada fica no carrinho sem chegar ao pedido ────────────

// Item que entra DEPOIS de a mutação ter lido a grade ainda assim chega ao
// pedido.
//
// A mutação lê o carrinho, fala com o ERP por ~1s e termina. Todo comentário
// dessa janela cai no CAS perdedor e desiste — o que só é seguro se quem ganhou
// enxergar a mudança. Quando o item é gravado depois da leitura, ninguém mais o
// aplicaria: numa live simulada de 15 compradores foram 11 unidades presas no
// carrinho, invisíveis para o ERP.
func TestItemQueChegaDepoisDaLeituraAindaAssimEntra(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	// Enquanto o PUT está no ar, o comprador comenta de novo.
	var umaVez sync.Once
	erp.antesDoPut = func() {
		umaVez.Do(func() { repo.definirItens("cart-1", item("p1", 6)) })
	}
	repo.definirItens("cart-1", item("p1", 3))

	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 6 {
		t.Errorf("reservado = %d, quero 6 — o item que entrou durante a chamada "+
			"tem de chegar ao pedido, senão ele fica só no carrinho", got)
	}
}

// Comentário durante a CRIAÇÃO do pedido também chega.
func TestComentarioDuranteACriacaoEntraNoPedido(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))

	// O ERP demora para criar; nesse meio-tempo chegam mais 4 unidades.
	var umaVez sync.Once
	erp.antesDaCriacao = func() {
		umaVez.Do(func() { repo.definirItens("cart-1", item("p1", 5)) })
	}

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if erp.criacoes != 1 {
		t.Fatalf("pedidos = %d, quero 1", erp.criacoes)
	}
	if got := erp.estoque("ext-p1").reservado; got != 5 {
		t.Errorf("reservado = %d, quero 5 — a reconciliação logo após a criação "+
			"existe exatamente para os comentários dessa janela", got)
	}
}

// E o caso comum não paga por isso: sem mudança, a reconciliação não gasta
// escrita nenhuma.
func TestReconciliacaoAposCriacaoNaoGastaEscritaAtoa(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	repo.criarCarrinho("cart-1", item("p1", 2))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if erp.puts != 0 {
		t.Errorf("PUTs = %d, quero 0 — nada mudou entre criar e reconciliar, e o "+
			"teto da conta é de 30 escritas por minuto", erp.puts)
	}
}

// Comentário que chega enquanto a criação está em voo não vira erro. Antes
// virava ("cart não está em 'open'") e o item ficava só no carrinho.
func TestComentarioDuranteCriacaoNaoViraErro(t *testing.T) {
	svc, repo, _, _ := montar(map[string]int{"ext-p1": 100})
	repo.criarCarrinho("cart-1", item("p1", 1))
	_, _ = repo.TransitionCartERPOrderState(context.Background(), "cart-1", OrderStateNone, OrderStateConverting)

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Errorf("comentário durante a criação virou erro: %v", err)
	}
}

// A grade é reconciliada ANTES de aprovar — a venda nunca fecha sobre um pedido
// que não é o carrinho.
//
// É a rede do sistema inteiro: a mutação converge por releitura, mas há um vão
// entre a última leitura e a liberação do estado, e um comentário que caia nele
// fica só no carrinho. Numa live simulada de 15 compradores foi uma unidade.
// No pagamento essa diferença deixa de ser tolerável.
func TestPagamentoReconciliaAGradeAntesDeAprovar(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	// O carrinho ganhou uma unidade que nunca chegou ao pedido.
	repo.definirItens("cart-1", item("p1", 3))

	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando: %v", err)
	}
	ped := erp.pedido(repo.carrinho("cart-1").externalOrderID)
	if ped.itens["ext-p1"] != 3 {
		t.Errorf("o pedido aprovado tem %d un. e o carrinho tem 3 — o comprador "+
			"pagou por algo diferente do que montou", ped.itens["ext-p1"])
	}
	if got := erp.estoque("ext-p1").reservado; got != 3 {
		t.Errorf("reservado = %d, quero 3", got)
	}
}

// E custa UM PUT por venda, não mais — o preço da garantia acima. Daqui não dá
// para saber o que o pedido tem sem perguntar, e perguntar custaria o mesmo que
// escrever.
func TestPagamentoGastaExatamenteUmPutDeGrade(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	putsAntes := erp.puts
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando: %v", err)
	}
	if got := erp.puts - putsAntes; got != 1 {
		t.Errorf("PUTs de grade no pagamento = %d, quero exatamente 1 — mais do que "+
			"isso é orçamento de escrita gasto por venda contra um teto de 30 por "+
			"minuto", got)
	}
}

// ─── 10. O que o lojista digita no pedido ───────────────────────────────────

// A escrita da grade é SUBSTITUIÇÃO. Sem reler antes, a linha que o lojista
// acrescentou pelo painel some na próxima mutação — e o estoque dela volta à
// venda enquanto ele acha que está comprometido.
//
// Medido contra a API real em 26/08/2026: pedido com 2 un. do produto A, lojista
// soma 3 un. do produto B, comentário seguinte faz o LiveCart reenviar só o A →
// HTTP 204, a linha B desaparece, e o `reservado` de B cai de 3 para 0.
func TestLinhaQueOLojistaAdicionouSobrevive(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 20, "ext-p2": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	// O lojista soma 3 unidades de outro produto pelo painel.
	erp.adicionarLinhaDoLojista(orderID, "ext-p2", 3, "cliente pediu por DM")
	if got := erp.estoque("ext-p2").reservado; got != 3 {
		t.Fatalf("preparo: reservado de p2 = %d, quero 3", got)
	}

	// Chega mais um comentário: o LiveCart reenvia a SUA grade.
	repo.definirItens("cart-1", item("p1", 5))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}

	ped := erp.pedido(orderID)
	if ped.itens["ext-p2"] != 3 {
		t.Errorf("a linha do lojista virou %d un. (quero 3) — reenviar a nossa grade "+
			"apagou o que ele digitou, sem aviso", ped.itens["ext-p2"])
	}
	if ped.itens["ext-p1"] != 5 {
		t.Errorf("a nossa linha = %d, quero 5", ped.itens["ext-p1"])
	}
	if got := erp.estoque("ext-p2").reservado; got != 3 {
		t.Errorf("reservado de p2 = %d, quero 3 — o estoque dele não pode voltar à "+
			"venda por causa de um comentário da compradora", got)
	}
}

// A nossa linha é SUBSTITUÍDA, não somada. Preservar a nossa própria linha e
// mandar a grade nova junto dobraria a quantidade.
func TestNossaLinhaNaoSeDuplicaNaFusao(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	for _, qtd := range []int{4, 7, 3} {
		repo.definirItens("cart-1", item("p1", qtd))
		if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
			t.Fatalf("mutação para %d: %v", qtd, err)
		}
		if got := erp.estoque("ext-p1").reservado; got != qtd {
			t.Fatalf("reservado = %d depois de mandar %d — a grade está somando em "+
				"vez de substituir", got, qtd)
		}
	}
}

// E remover um item do carrinho continua removendo do pedido: a linha é nossa,
// tem o nosso marcador, e sai.
func TestRemoverItemAindaRemoveComAFusaoLigada(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 20, "ext-p2": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3), item("p2", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")

	repo.definirItens("cart-1", item("p1", 3))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := erp.estoque("ext-p2").reservado; got != 0 {
		t.Errorf("reservado de p2 = %d, quero 0 — a linha era NOSSA e saiu do "+
			"carrinho; preservá-la seria não devolver o estoque nunca", got)
	}
}

// Falha de leitura NÃO vira escrita cega. Adiar a mutação atrasa o ajuste da
// reserva e a próxima tentativa a faz; escrever sem saber apaga o trabalho de
// alguém para sempre.
func TestLeituraQueFalhaNaoViraEscritaCega(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 20, "ext-p2": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID
	erp.adicionarLinhaDoLojista(orderID, "ext-p2", 4, "do lojista")

	erp.falharLeituraDeGrade = errors.New("500 do ERP")
	putsAntes := erp.puts
	repo.definirItens("cart-1", item("p1", 9))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err == nil {
		t.Fatal("a mutação devia falhar em vez de escrever sem saber o que há lá")
	}
	if erp.puts != putsAntes {
		t.Errorf("escreveu %d vez(es) sem conseguir ler antes", erp.puts-putsAntes)
	}
	if got := erp.pedido(orderID).itens["ext-p2"]; got != 4 {
		t.Errorf("a linha do lojista virou %d — a falha de leitura a destruiu", got)
	}

	// Com o ERP de volta, a mutação acontece e preserva.
	erp.falharLeituraDeGrade = nil
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("segunda tentativa: %v", err)
	}
	ped := erp.pedido(orderID)
	if ped.itens["ext-p1"] != 9 || ped.itens["ext-p2"] != 4 {
		t.Errorf("grade final = %v, quero p1=9 e p2=4", ped.itens)
	}
}

// Linha SEM marcador de um produto que ESTÁ no carrinho é tratada como nossa.
//
// É o caso dos pedidos criados antes do marcador existir: preservá-la e mandar a
// nossa junto dobraria a quantidade. O preço é a ambiguidade que sobra — se o
// lojista somar unidades de um produto que a compradora já pediu, a nossa
// quantidade vence, porque ela vem do carrinho.
func TestLinhaAntigaSemMarcadorNaoDobraAQuantidade(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	// Encena o pedido legado: a linha existe sem marcador nenhum.
	erp.limparMarcadores(orderID)

	repo.definirItens("cart-1", item("p1", 6))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 6 {
		t.Errorf("reservado = %d, quero 6 — a linha antiga foi somada à nova em vez "+
			"de substituída", got)
	}
}

// A fusão custa UMA leitura por mutação. É o preço de nunca apagar o trabalho do
// lojista, e ele precisa ficar visível: numa live de 450 comentários são 450
// requisições a mais contra um teto de 30 por minuto.
func TestFusaoCustaUmaLeituraPorMutacao(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	leiturasAntes := erp.leiturasDeGrade
	for _, qtd := range []int{2, 3, 4} {
		repo.definirItens("cart-1", item("p1", qtd))
		_ = svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
	}
	if got := erp.leiturasDeGrade - leiturasAntes; got != 3 {
		t.Errorf("leituras = %d para 3 mutações, quero 3 — uma a mais por mutação é "+
			"o custo aceito; mais que isso não é", got)
	}
}

// ─── 1b. O modo de reserva decide QUANDO o pedido nasce ─────────────────────

// No modo LOCAL o comentário não cria pedido: quem segura a peça durante a live
// é o contador do LiveCart. Criar cedo custaria duas requisições por compradora
// contra o teto de 3 req/s do Bling sem reservar nada, porque numa conta sem a
// Reserva de estoque ligada pedido em aberto não mexe no saldo.
func TestModoLocalNaoCriaOPedidoNoComentario(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.provider = "bling"
	repo.metadata = map[string]any{ChaveModoDeReserva: string(ReservaSomenteLocal)}
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário no modo local: %v", err)
	}

	if erp.criacoes != 0 {
		t.Errorf("pedidos criados = %d, quero 0 — no modo local o pedido nasce no pagamento", erp.criacoes)
	}
	if c := repo.carrinho("cart-1"); c.state != OrderStateNone || c.externalOrderID != "" {
		t.Errorf("carrinho ficou em %q com pedido %q, quero 'none' e vazio", c.state, c.externalOrderID)
	}
	if est := erp.estoque("ext-p1"); est.saldo != 5 || est.reservado != 0 {
		t.Errorf("saldo=%d reservado=%d, quero 5 e 0 — o modo local não toca o ERP na live",
			est.saldo, est.reservado)
	}
}

// O CONTRÁRIO da regra acima, e o que mais importa aqui: o carrinho PAGO cria o
// pedido em qualquer modo. A primeira versão deste portão fechava as duas
// portas juntas, e o carrinho pago — que chega em 'none' — teria ficado sem
// pedido para sempre. A venda existiu; o pedido tem de existir.
func TestModoLocalAindaAssimCriaOPedidoQuandoOCarrinhoEhPago(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.provider = "bling"
	repo.metadata = map[string]any{ChaveModoDeReserva: string(ReservaSomenteLocal)}
	repo.criarCarrinho("cart-1", item("p1", 1))

	ctx := context.Background()
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if erp.criacoes != 0 {
		t.Fatalf("o comentário criou %d pedido(s) — o teste anterior já deveria ter pegado isso", erp.criacoes)
	}

	// Agora a compradora paga.
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando o carrinho pago: %v", err)
	}

	if erp.criacoes != 1 {
		t.Fatalf("pedidos criados no pagamento = %d, quero 1 — sem isso a venda paga "+
			"não existe no ERP do lojista", erp.criacoes)
	}
	if c := repo.carrinho("cart-1"); c.externalOrderID == "" {
		t.Errorf("carrinho pago ficou sem external_order_id (estado %q)", c.state)
	}
}

// E o Tiny não muda: o pedido continua nascendo no primeiro comentário, que é
// como a live fatura hoje.
func TestTinyContinuaCriandoOPedidoNoComentario(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.provider = "tiny" // sem metadata nenhum, como toda integração Tiny em produção
	repo.criarCarrinho("cart-1", item("p1", 1))

	if err := svc.ReserveStockInERP(context.Background(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("primeiro comentário: %v", err)
	}
	if erp.criacoes != 1 {
		t.Fatalf("pedidos criados = %d, quero 1 — o Tiny reserva PELO pedido; sem ele "+
			"a live para de segurar peça", erp.criacoes)
	}
}

// Um ERP que RECUSA mudar a situação não pode travar a venda.
//
// O pedido já existe lá, com itens e pagamento. Abortar deixaria o pior estado
// possível: pedido certo no ERP do lojista e carrinho eternamente "não
// confirmado" no LiveCart. Só a recusa explícita (ErrOperationNotSupported) é
// tolerada — falha de rede continua sendo falha.
func TestRecusaDeSituacaoNaoTravaAVendaJaPaga(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 1))
	ctx := context.Background()

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	erp.recusarSituacao = true

	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("o pagamento falhou por causa da situação: %v", err)
	}
	if c := repo.carrinho("cart-1"); c.state != OrderStateConfirmed {
		t.Errorf("carrinho ficou em %q, quero 'confirmed' — a venda aconteceu", c.state)
	}
}

// E o contrário: uma falha de verdade continua abortando.
func TestFalhaDeVerdadeNaSituacaoAindaAbortaAConfirmacao(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5})
	repo.criarCarrinho("cart-1", item("p1", 1))
	ctx := context.Background()

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	erp.falharSituacao = true

	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err == nil {
		t.Error("o pagamento passou apesar de a situação ter falhado de verdade — " +
			"a tolerância vazou para além da recusa explícita")
	}
}
