package erp

// Juntar compras num pedido só.
//
// "A compradora pagou na live de segunda, pediu mais uma coisa na quinta, e sai
// numa caixa só." O pedido pago continua recebendo item — até o faturamento,
// que é quando ele vira nota e a porta fecha.
//
// O ERP não impõe esse limite: em 26/08/2026 ele aceitou (204) editar os itens
// de um pedido em situação "Faturada". A recusa é nossa, e é isto que estes
// testes travam.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// paraJuntar deixa um carrinho pago, com pedido, pronto para receber mais.
func paraJuntar(t *testing.T, saldos map[string]int) (*Service, *repoSimulado, *erpComParcelas) {
	t.Helper()
	svc, repo, erp := montarParcelas(saldos)
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2)) // R$ 40
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria"); err != nil {
		t.Fatalf("primeira compra: %v", err)
	}
	pagar(t, svc, repo, "cart-1", 4000)
	return svc, repo, erp
}

// ─── A regra ────────────────────────────────────────────────────────────────

// Segunda: paga R$ 40. Quinta: pede mais. Um pedido só, e o ERP recebe o item.
func TestPedidoPagoRecebeItemDeOutroEvento(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID

	// Evento NOVO, mesmo carrinho — é o que a resolução do carrinho eterno faz.
	repo.acrescentarItem("cart-1", item("p2", 3))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-2", "p2", 3, 1500, "@maria"); err != nil {
		t.Fatalf("segunda compra num pedido pago: %v", err)
	}

	if novo := repo.carrinho("cart-1").externalOrderID; novo != orderID {
		t.Errorf("abriu um pedido novo (%s ≠ %s) — a compra de quinta tinha de "+
			"entrar no pedido de segunda, para sair um frete só", novo, orderID)
	}
	if q := erp.quantidadeNoPedido(orderID, "ext-p2"); q != 3 {
		t.Errorf("o pedido tem %d un. do produto novo, quero 3 — o item ficou no "+
			"carrinho e nunca chegou ao ERP", q)
	}
	if est := repo.carrinho("cart-1").state; est != OrderStateConfirmed {
		t.Errorf("o carrinho terminou em %q, quero %q — a mutação não pode "+
			"rebaixar um pedido pago a 'open'", est, OrderStateConfirmed)
	}
}

// E o dinheiro fica separado: R$ 40 pagos, R$ 45 a pagar.
func TestJuntarNoPedidoPagoSeparaOQueFaltaPagar(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID

	repo.acrescentarItem("cart-1", item("p2", 3)) // 3 × R$ 20 = R$ 60
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-2", "p2", 3, 2000, "@maria"); err != nil {
		t.Fatalf("segunda compra: %v", err)
	}

	if falta := repo.faltaPagar("cart-1"); falta != 6000 {
		t.Errorf("falta pagar %d, quero 6000 — o que entrou depois do pagamento "+
			"tem de aparecer separado", falta)
	}
	p := erp.parcelas[orderID]
	if len(p) != 2 {
		t.Fatalf("o pedido tem %d parcela(s), quero 2 — é onde o lojista lê o "+
			"que está pago e o que falta", len(p))
	}
	if p[0].AmountCents != 4000 {
		t.Errorf("a parcela paga = %d, quero 4000 — o ERP redistribui o total "+
			"pelas parcelas a cada item novo e passa a afirmar que ela pagou "+
			"o valor cheio", p[0].AmountCents)
	}
	if p[0].AmountCents+p[1].AmountCents != 10000 {
		t.Errorf("as parcelas somam %d, quero 10000 (o total do pedido) — soma que "+
			"não fecha é reescrita em silêncio", p[0].AmountCents+p[1].AmountCents)
	}
}

// ─── O portão ───────────────────────────────────────────────────────────────

// Faturado: a nota existe. Somar item nela seria emitir nota errada.
func TestPedidoFaturadoNaoRecebeMaisItem(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID
	repo.definirStatusERP("cart-1", string(providers.ERPOrderStatusFaturado))

	repo.acrescentarItem("cart-1", item("p2", 3))
	err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
	if !errors.Is(err, ErrPedidoFaturado) {
		t.Fatalf("erro = %v, quero ErrPedidoFaturado — o ERP aceita essa edição "+
			"(204 medido em 26/08/2026), então quem recusa somos nós", err)
	}
	if q := erp.quantidadeNoPedido(orderID, "ext-p2"); q != 0 {
		t.Errorf("o item entrou num pedido faturado (%d un.)", q)
	}
	if est := repo.carrinho("cart-1").state; est != OrderStateConfirmed {
		t.Errorf("a recusa deixou o carrinho em %q — tinha de continuar em %q",
			est, OrderStateConfirmed)
	}
}

// A porta fecha nas situações que vêm DEPOIS do faturamento também: elas todas
// têm nota emitida.
func TestSituacoesPosFaturamentoTambemFechamAPorta(t *testing.T) {
	fechadas := []providers.ERPOrderStatus{
		providers.ERPOrderStatusPreparandoEnvio,
		providers.ERPOrderStatusFaturado,
		providers.ERPOrderStatusProntoEnvio,
		providers.ERPOrderStatusEnviado,
		providers.ERPOrderStatusEntregue,
		providers.ERPOrderStatusNaoEntregue,
		providers.ERPOrderStatusCancelado,
	}
	abertas := []providers.ERPOrderStatus{
		providers.ERPOrderStatusAberto,
		providers.ERPOrderStatusAprovado,
		providers.ERPOrderStatusDadosIncompletos,
	}
	for _, s := range fechadas {
		if !s.FechadoParaNovosItens() {
			t.Errorf("%q devia fechar a porta: a nota já existe", s)
		}
	}
	for _, s := range abertas {
		if s.FechadoParaNovosItens() {
			t.Errorf("%q não devia fechar a porta — perderia a venda de uma "+
				"cliente que só queria somar uma peça", s)
		}
	}
}

// "Preparando envio" NÃO é uma janela de edição.
//
// O nome sugere alguém montando a caixa, e a lista do enum o coloca antes de
// "Faturada" — as duas coisas enganam. Na operação o pedido só entra em preparo
// depois de a nota sair, então aqui o documento fiscal já existe.
func TestPreparandoEnvioJaTemNotaENaoRecebeItem(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID
	repo.definirStatusERP("cart-1", string(providers.ERPOrderStatusPreparandoEnvio))

	repo.acrescentarItem("cart-1", item("p2", 2))
	err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
	if !errors.Is(err, ErrPedidoFaturado) {
		t.Fatalf("erro = %v, quero ErrPedidoFaturado — em preparo a nota já saiu", err)
	}
	if q := erp.quantidadeNoPedido(orderID, "ext-p2"); q != 0 {
		t.Errorf("o item entrou (%d un.) num pedido que já tem nota", q)
	}
}

// Situação ainda desconhecida (webhook a caminho) não pode recusar a venda: o
// pedido acabou de ser pago e nasce 'aprovado'.
func TestSituacaoDesconhecidaNaoRecusaAVenda(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID
	repo.definirStatusERP("cart-1", "")

	repo.acrescentarItem("cart-1", item("p2", 1))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("recusou por não saber a situação: %v", err)
	}
	if q := erp.quantidadeNoPedido(orderID, "ext-p2"); q != 1 {
		t.Errorf("o pedido tem %d un., quero 1", q)
	}
}

// ─── Corridas ───────────────────────────────────────────────────────────────

// Rajada de comentários num pedido JÁ PAGO: um pedido só, nenhuma unidade
// perdida, e a divisão do dinheiro fecha no fim.
func TestRajadaNoPedidoPagoNaoPerdeUnidade(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 500, "ext-p2": 500})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID

	const comentarios = 12
	pronto := make(chan struct{})
	fim := make(chan error, comentarios)
	for i := 0; i < comentarios; i++ {
		go func() {
			<-pronto
			repo.acrescentarItem("cart-1", item("p2", 1))
			fim <- svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-2", "p2", 1, 1000, "@maria")
		}()
	}
	close(pronto)
	for i := 0; i < comentarios; i++ {
		if err := <-fim; err != nil {
			t.Fatalf("comentário %d: %v", i, err)
		}
	}
	// A última mutação a soltar o estado reconfere; dá tempo a ela.
	time.Sleep(50 * time.Millisecond)

	if novo := repo.carrinho("cart-1").externalOrderID; novo != orderID {
		t.Errorf("a rajada abriu um segundo pedido: %s ≠ %s", novo, orderID)
	}
	noCarrinho := repo.quantidadeNoCarrinho("cart-1", "p2")
	if q := erp.quantidadeNoPedido(orderID, "ext-p2"); q != noCarrinho {
		t.Errorf("o pedido tem %d un. e o carrinho %d — unidade que ficou no "+
			"banco e nunca chegou ao ERP é venda que o lojista não separa",
			q, noCarrinho)
	}
	if est := repo.carrinho("cart-1").state; est != OrderStateConfirmed {
		t.Errorf("a rajada deixou o carrinho em %q, quero %q", est, OrderStateConfirmed)
	}
	p := erp.parcelas[orderID]
	if len(p) == 2 && p[0].AmountCents != 4000 {
		t.Errorf("depois da rajada a parcela paga virou %d, quero 4000", p[0].AmountCents)
	}
}

// Faturamento no meio da rajada: o que entrou antes fica, o que chega depois é
// recusado — e nunca um pedido faturado com item a mais.
func TestFaturamentoNoMeioDaRajadaFechaAPorta(t *testing.T) {
	svc, repo, erp := paraJuntar(t, map[string]int{"ext-p1": 500, "ext-p2": 500})
	ctx := context.Background()
	orderID := repo.carrinho("cart-1").externalOrderID

	const comentarios = 10
	pronto := make(chan struct{})
	fim := make(chan error, comentarios)
	for i := 0; i < comentarios; i++ {
		go func(i int) {
			<-pronto
			if i == comentarios/2 {
				repo.definirStatusERP("cart-1", string(providers.ERPOrderStatusFaturado))
			}
			repo.acrescentarItem("cart-1", item("p2", 1))
			fim <- svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-2", "p2", 1, 1000, "@maria")
		}(i)
	}
	close(pronto)
	var recusas int
	for i := 0; i < comentarios; i++ {
		if err := <-fim; err != nil {
			if !errors.Is(err, ErrPedidoFaturado) {
				t.Fatalf("comentário %d falhou por outro motivo: %v", i, err)
			}
			recusas++
		}
	}
	if recusas == 0 {
		t.Skip("o faturamento chegou depois de todos: nada a provar nesta rodada")
	}
	noPedido := erp.quantidadeNoPedido(orderID, "ext-p2")
	noCarrinho := repo.quantidadeNoCarrinho("cart-1", "p2")
	if noPedido > noCarrinho {
		t.Errorf("o pedido faturado tem %d un. e o carrinho só %d — entrou item "+
			"depois da nota", noPedido, noCarrinho)
	}
}

// ─── A nota fiscal, medida ──────────────────────────────────────────────────
//
// Gerar a nota (`POST /pedidos/{id}/gerar-nota-fiscal`) foi medido em
// 26/08/2026 na conta real:
//
//	antes:   situacao 0 (Em aberto)      idNotaFiscal 0
//	depois:  situacao 4 (Preparando envio) idNotaFiscal 368093855
//
// A situação vai para 4, NUNCA para 1 ("Faturada"). Quem esperasse o 1 deixaria
// a porta aberta com a nota já emitida — que é o erro que eu tinha cometido ao
// ler o nome "Preparando envio" como "ainda dá tempo".

// erpComNota encena a releitura recusando o pedido que já tem nota.
type erpComNota struct {
	*erpComParcelas
	comNota map[string]bool
}

func (e *erpComNota) GetOrderItems(ctx context.Context, orderID string) ([]providers.ERPOrderItem, error) {
	if e.comNota[orderID] {
		return nil, fmt.Errorf("pedido %s tem a nota 999: %w", orderID, providers.ErrPedidoComNotaFiscal)
	}
	return e.erpComParcelas.GetOrderItems(ctx, orderID)
}

// A recusa da releitura vira ErrPedidoFaturado, não "erro de leitura".
//
// A diferença importa: erro de leitura ADIA a escrita e o próximo comentário
// tenta de novo, para sempre. Nota emitida é uma porta fechada, e o chamador
// precisa saber disso para abrir um pedido novo em vez de insistir.
func TestNotaFiscalNaReleituraFechaAPortaEmVezDeAdiar(t *testing.T) {
	e := novoERPSimulado(map[string]int{"ext-p1": 50, "ext-p2": 50})
	r := novoRepoSimulado()
	comNota := &erpComNota{erpComParcelas: &erpComParcelas{erpSimulado: e}, comNota: map[string]bool{}}
	svc := NewService(r, &colabSimulado{erp: comNota, repo: r}, zap.NewNop())
	svc.SetOrderStatusRepository(r)
	svc.SetWriteLimits(limitesAbertos())

	ctx := context.Background()
	r.criarCarrinho("cart-1", item("p1", 2))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria"); err != nil {
		t.Fatalf("primeira compra: %v", err)
	}
	orderID := r.carrinho("cart-1").externalOrderID
	// O lojista emite a nota pelo painel. Nenhum webhook chegou ainda: a
	// situação no carrinho continua dizendo 'aberto'.
	comNota.comNota[orderID] = true

	r.acrescentarItem("cart-1", item("p2", 3))
	err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
	if !errors.Is(err, ErrPedidoFaturado) {
		t.Fatalf("erro = %v, quero ErrPedidoFaturado — a nota está emitida e a "+
			"situação local ainda não sabe; o idNotaFiscal do próprio pedido é "+
			"o único sinal que não depende de webhook", err)
	}
	if q := comNota.quantidadeNoPedido(orderID, "ext-p2"); q != 0 {
		t.Errorf("escreveu %d un. num pedido que já virou nota", q)
	}
}

// A situação que a emissão da nota produz é 'preparando_envio', e ela fecha a
// porta. Esta é a tradução direta da medição.
func TestSituacaoQueAEmissaoDaNotaProduzFechaAPorta(t *testing.T) {
	produzidaPelaNota := providers.ERPOrderStatusPreparandoEnvio
	if code, ok := providers.SituacaoFromERPOrderStatus(produzidaPelaNota); !ok || code != 4 {
		t.Fatalf("preparando_envio = %d, quero 4 — é o código medido depois de "+
			"gerar a nota", code)
	}
	if !produzidaPelaNota.FechadoParaNovosItens() {
		t.Error("a situação que a emissão da nota produz não fecha a porta — " +
			"seria receber item com a nota já emitida")
	}
	// E a situação 1, que o nome sugere, NÃO é a que a emissão produz.
	faturada, ok := providers.ERPOrderStatusFromSituacao(1)
	if !ok || faturada != providers.ERPOrderStatusFaturado {
		t.Fatalf("situação 1 = %q", faturada)
	}
	if !faturada.FechadoParaNovosItens() {
		t.Error("'faturado' também tem de fechar a porta")
	}
}
