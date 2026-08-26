package erp

// O dinheiro do pedido pago.
//
// O ERP redistribui o total pelas parcelas a cada mudança de item e afirma, em
// silêncio, que a compradora pagou o valor novo. O que estes testes travam é a
// verdade: uma parcela com o que entrou, outra com o que falta.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// erpComParcelas acrescenta ao simulador o controle explícito de parcelas, com a
// regra medida: a soma é forçada ao total do pedido.
type erpComParcelas struct {
	*erpSimulado
	parcelas      map[string][]providers.ERPInstallment
	escritas      int
	falharEscrita error
	totalForcado  int64
	usarForcado   bool
}

func (e *erpComParcelas) GetOrderTotal(_ context.Context, orderID string) (int64, bool, error) {
	if e.usarForcado {
		return e.totalForcado, false, nil
	}
	p := e.pedido(orderID)
	if p == nil {
		return 0, false, fmt.Errorf("pedido %s não existe", orderID)
	}
	var total int64
	e.mu.Lock()
	for produto, q := range p.itens {
		_ = produto
		total += int64(q) * 2000
	}
	e.mu.Unlock()
	return total, false, nil
}

func (e *erpComParcelas) SetOrderInstallments(_ context.Context, orderID string, parcelas []providers.ERPInstallment) error {
	e.escritas++
	if e.falharEscrita != nil {
		return e.falharEscrita
	}
	if e.parcelas == nil {
		e.parcelas = map[string][]providers.ERPInstallment{}
	}
	e.parcelas[orderID] = append([]providers.ERPInstallment(nil), parcelas...)
	return nil
}

func montarParcelas(saldos map[string]int) (*Service, *repoSimulado, *erpComParcelas) {
	e := novoERPSimulado(saldos)
	r := novoRepoSimulado()
	comParcelas := &erpComParcelas{erpSimulado: e}
	c := &colabSimulado{erp: comParcelas, repo: r}
	s := NewService(r, c, zap.NewNop())
	s.SetOrderStatusRepository(r)
	s.SetWriteLimits(limitesAbertos())
	return s, r, comParcelas
}

func pagar(t *testing.T, svc *Service, repo *repoSimulado, cartID string, centavos int64) {
	t.Helper()
	quando := time.Now().Add(-24 * time.Hour)
	if entrou := repo.pagarCarrinho(cartID, quando); entrou != centavos {
		t.Fatalf("o pagamento cobriu %d e o teste esperava %d — a conta do que "+
			"faltava pagar não bate com o carrinho montado", entrou, centavos)
	}
	if err := svc.ConfirmERPOrderPayment(context.Background(), cartID, "loja-1", &providers.PaymentStatus{
		PaymentID: "PAY-1", PaymentMethod: "pix", Amount: centavos, PaidAt: &quando,
	}); err != nil {
		t.Fatalf("pagamento: %v", err)
	}
}

// ─── O caso que motivou tudo ────────────────────────────────────────────────

// Ela pagou R$ 40 na live de segunda. Na quinta o lojista soma R$ 105 em itens
// no mesmo pedido. O pedido tem de dizer: R$ 40 pagos, R$ 105 a pagar.
func TestPedidoPagoQueGanhaItemSeparaPagoDeAPagar(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2)) // 2 × R$ 20 = R$ 40
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	pagar(t, svc, repo, "cart-1", 4000)

	// O lojista soma itens: o pedido passa a valer R$ 145 (aqui, 5 un. × R$ 20
	// mais as 2 originais = R$ 140; o número exato não importa, a divisão sim).
	orderID := repo.carrinho("cart-1").externalOrderID
	erp.adicionarLinhaDoLojista(orderID, "ext-p2", 5, "cliente pediu por DM")

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if !split.Reescrito {
		t.Fatalf("não reescreveu as parcelas: %+v", split)
	}
	if split.PagoCents != 4000 {
		t.Errorf("pago = %d, quero 4000 — é o que o gateway registrou", split.PagoCents)
	}
	if split.SaldoCents != split.TotalCents-4000 {
		t.Errorf("saldo = %d, quero %d", split.SaldoCents, split.TotalCents-4000)
	}

	p := erp.parcelas[orderID]
	if len(p) != 2 {
		t.Fatalf("parcelas = %d, quero 2 (paga e a pagar)", len(p))
	}
	if p[0].AmountCents+p[1].AmountCents != split.TotalCents {
		t.Errorf("as parcelas somam %d e o pedido vale %d — o ERP força a soma ao "+
			"total, e uma divisão que não fecha é reescrita em silêncio",
			p[0].AmountCents+p[1].AmountCents, split.TotalCents)
	}
	if p[0].AmountCents != 4000 {
		t.Errorf("a primeira parcela = %d, quero 4000 (o que entrou)", p[0].AmountCents)
	}
	if !contains(p[0].Note, "PAGO") || !contains(p[1].Note, "A PAGAR") {
		t.Errorf("as observações não dizem ao lojista o que é o quê: %q / %q", p[0].Note, p[1].Note)
	}
}

// Pedido que vale exatamente o que foi pago não é tocado — é o caso normal, e o
// mais comum de todos.
func TestPedidoQueValeOQueFoiPagoNaoEhTocado(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3)) // R$ 60
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")
	pagar(t, svc, repo, "cart-1", 6000)

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.Reescrito {
		t.Error("reescreveu as parcelas de um pedido que já batia")
	}
	if erp.escritas != 0 {
		t.Errorf("gastou %d escrita(s) à toa — o teto da conta é 30 por minuto", erp.escritas)
	}
}

// Pedido que ficou MENOR que o valor pago: existe crédito, o ERP não consegue
// registrar parcelas que somem mais que o total, e qualquer número seria mentira.
// Não escreve, e sobe para alguém decidir.
func TestPedidoMenorQueOPagoNaoEhReescrito(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 5)) // R$ 100
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 5, 2000, "@maria")
	pagar(t, svc, repo, "cart-1", 10000)

	// O lojista tira itens: o pedido passa a valer R$ 40.
	erp.usarForcado = true
	erp.totalForcado = 4000

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.Reescrito {
		t.Error("reescreveu — e qualquer número gravado aqui seria mentira")
	}
	if erp.escritas != 0 {
		t.Errorf("escreveu %d vez(es) num caso que pede decisão humana", erp.escritas)
	}
	if split.Motivo == "" {
		t.Error("não disse por que se recusou")
	}
	if split.SaldoCents >= 0 {
		t.Errorf("saldo = %d, quero negativo — é o crédito a devolver", split.SaldoCents)
	}
}

// Carrinho que ainda não foi pago não tem o que separar.
func TestCarrinhoNaoPagoNaoTemParcelaASeparar(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split != nil {
		t.Errorf("agiu sobre carrinho não pago: %+v", split)
	}
	if erp.escritas != 0 {
		t.Error("escreveu parcelas num pedido sem pagamento")
	}
}

// Sem o retrato do gateway não dá para afirmar quanto entrou — e inventar é pior
// do que deixar o pedido como está.
func TestSemRetratoDoGatewayNaoInventaValor(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("confirmando sem retrato: %v", err)
	}
	repo.snapshot = nil

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split != nil && split.Reescrito {
		t.Error("inventou um valor pago sem ter o retrato do gateway")
	}
	if erp.escritas != 0 {
		t.Error("escreveu parcelas sem saber quanto entrou")
	}
}

// Repetir a recomposição não muda nada: a divisão é uma função do total e do
// valor pago, e os dois não mudam entre chamadas.
func TestRecomposicaoRepetidaEhIdempotente(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	pagar(t, svc, repo, "cart-1", 4000)
	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-p2", 3, "extra")

	var primeira []providers.ERPInstallment
	for i := 0; i < 3; i++ {
		if _, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1"); err != nil {
			t.Fatalf("recomposição %d: %v", i, err)
		}
		agora := erp.parcelas[repo.carrinho("cart-1").externalOrderID]
		if i == 0 {
			primeira = agora
			continue
		}
		if len(agora) != len(primeira) || agora[0].AmountCents != primeira[0].AmountCents ||
			agora[1].AmountCents != primeira[1].AmountCents {
			t.Errorf("a recomposição %d deu valores diferentes: %v vs %v", i, agora, primeira)
		}
	}
}

// A falha do ERP sobe — não pode ser engolida, porque o pedido fica afirmando
// que ela pagou mais do que pagou.
func TestFalhaAoReescreverParcelasSobe(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	pagar(t, svc, repo, "cart-1", 4000)
	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-p2", 3, "extra")
	erp.falharEscrita = fmt.Errorf("503 do ERP")

	if _, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1"); err == nil {
		t.Error("engoliu a falha — o pedido segue dizendo que ela pagou o valor novo")
	}
}
