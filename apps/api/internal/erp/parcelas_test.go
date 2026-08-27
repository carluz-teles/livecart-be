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

// ─── Desconto de cupom e de PIX ─────────────────────────────────────────────
//
// O total do pedido no ERP é o preço CHEIO — `valorDesconto` é gravável só na
// criação, e o pedido nasce no primeiro comentário, muito antes de existir
// cupom ou forma de pagamento. Então o dinheiro que entra é MENOR que o total,
// sempre, quando há desconto. Sem uma parcela dizendo isso, a diferença viraria
// saldo devedor e o pedido cobraria o que ninguém deve.

// somaParcelas confere a lei que o ERP impõe em silêncio.
func somaParcelas(p []providers.ERPInstallment) int64 {
	var t int64
	for _, x := range p {
		t += x.AmountCents
	}
	return t
}

func achaNota(p []providers.ERPInstallment, sub string) *providers.ERPInstallment {
	for i := range p {
		if contains(p[i].Note, sub) {
			return &p[i]
		}
	}
	return nil
}

// PIX com desconto: entra menos do que o preço cheio, e o que falta NÃO é dívida.
func TestDescontoPixViraParcelaEmVezDeSaldoDevedor(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 5)) // preço cheio: R$ 100
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 5, 2000, "@maria")

	// PIX com 5% de desconto: entram R$ 95 cobrindo R$ 100 de preço cheio.
	repo.cobrar("cart-1", time.Now().Add(-time.Hour), 9500, "pix")
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatalf("pagamento: %v", err)
	}

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.SaldoCents != 0 {
		t.Errorf("saldo = %d, quero 0 — os R$ 5 de desconto não são dívida da "+
			"compradora, e cobrá-los seria cobrar o que ninguém deve", split.SaldoCents)
	}
	if split.DescontoCents != 500 {
		t.Errorf("desconto = %d, quero 500", split.DescontoCents)
	}

	p := erp.parcelas[repo.carrinho("cart-1").externalOrderID]
	if somaParcelas(p) != split.TotalCents {
		t.Errorf("as parcelas somam %d e o pedido vale %d — soma que não fecha é "+
			"substituída pelo total em silêncio", somaParcelas(p), split.TotalCents)
	}
	if d := achaNota(p, "DESCONTO"); d == nil || d.AmountCents != 500 {
		t.Errorf("faltou a parcela de desconto de R$ 5: %+v", p)
	}
	if a := achaNota(p, "A PAGAR"); a != nil {
		t.Errorf("criou dívida de %d que não existe — é o desconto disfarçado", a.AmountCents)
	}
}

// Desconto E saldo ao mesmo tempo: os dois convivem, e a soma continua fechando.
func TestDescontoESaldoConvivemNaMesmaDivisao(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 5)) // R$ 100
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 5, 2000, "@maria")
	repo.cobrar("cart-1", time.Now().Add(-time.Hour), 9500, "pix") // 5% off
	_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)

	// Na quinta ela pede mais R$ 40.
	repo.acrescentarItem("cart-1", item("p2", 2))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("acrescentando item: %v", err)
	}

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.PagoCents != 9500 || split.DescontoCents != 500 || split.SaldoCents != 4000 {
		t.Errorf("pago=%d desconto=%d saldo=%d, quero 9500/500/4000",
			split.PagoCents, split.DescontoCents, split.SaldoCents)
	}
	p := erp.parcelas[repo.carrinho("cart-1").externalOrderID]
	if somaParcelas(p) != 14000 {
		t.Errorf("as parcelas somam %d, quero 14000 (o total do pedido)", somaParcelas(p))
	}
	if achaNota(p, "DESCONTO") == nil || achaNota(p, "A PAGAR") == nil || achaNota(p, "PAGO") == nil {
		t.Errorf("o extrato não tem as três linhas que o lojista precisa ler: %+v", p)
	}
}

// ─── Vários pagamentos até quitar ───────────────────────────────────────────

// Cada cobrança é uma parcela "PAGO", com a data dela. É isso que transforma o
// pedido num extrato em vez de um total mudo.
func TestCadaCobrancaViraUmaParcelaPaga(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50, "ext-p2": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2)) // R$ 40
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	repo.cobrar("cart-1", time.Now().Add(-72*time.Hour), -1, "pix")
	_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)

	// Quinta: mais R$ 60, e ela paga o saldo na hora.
	repo.acrescentarItem("cart-1", item("p2", 3))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("acrescentando: %v", err)
	}
	repo.cobrar("cart-1", time.Now(), -1, "pix")

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.Pagamentos != 2 {
		t.Errorf("pagamentos = %d, quero 2", split.Pagamentos)
	}
	if split.SaldoCents != 0 {
		t.Errorf("saldo = %d, quero 0 — o pedido foi quitado", split.SaldoCents)
	}

	p := erp.parcelas[repo.carrinho("cart-1").externalOrderID]
	var pagas int
	for _, x := range p {
		if contains(x.Note, "PAGO") {
			pagas++
		}
	}
	if pagas != 2 {
		t.Errorf("parcelas PAGO = %d, quero 2 — uma por cobrança, com a data de "+
			"cada uma; um total só não conta essa história: %+v", pagas, p)
	}
	if somaParcelas(p) != 10000 {
		t.Errorf("as parcelas somam %d, quero 10000", somaParcelas(p))
	}
	if achaNota(p, "A PAGAR") != nil {
		t.Error("deixou saldo devedor num pedido quitado")
	}
}

// Ir pagando aos poucos: a cada rodada a soma fecha e o saldo encolhe.
func TestPagamentosSucessivosAteQuitar(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 500, "ext-p2": 500})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	repo.cobrar("cart-1", time.Now().Add(-96*time.Hour), -1, "pix")
	_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)

	saldoAnterior := int64(-1)
	for rodada := 1; rodada <= 4; rodada++ {
		repo.acrescentarItem("cart-1", item("p2", 1))
		if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
			t.Fatalf("rodada %d: %v", rodada, err)
		}
		split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
		if err != nil {
			t.Fatalf("rodada %d, recompondo: %v", rodada, err)
		}
		if split.SaldoCents != 2000 {
			t.Errorf("rodada %d: saldo = %d, quero 2000 (a peça recém-pedida)",
				rodada, split.SaldoCents)
		}
		p := erp.parcelas[repo.carrinho("cart-1").externalOrderID]
		if somaParcelas(p) != split.TotalCents {
			t.Errorf("rodada %d: parcelas somam %d e o pedido vale %d",
				rodada, somaParcelas(p), split.TotalCents)
		}

		repo.cobrar("cart-1", time.Now(), -1, "pix")
		quitado, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
		if err != nil {
			t.Fatalf("rodada %d, quitando: %v", rodada, err)
		}
		if quitado.SaldoCents != 0 {
			t.Errorf("rodada %d: sobrou %d depois de quitar", rodada, quitado.SaldoCents)
		}
		if quitado.Pagamentos != rodada+1 {
			t.Errorf("rodada %d: pagamentos = %d, quero %d", rodada, quitado.Pagamentos, rodada+1)
		}
		if saldoAnterior >= 0 && quitado.TotalCents <= saldoAnterior {
			t.Errorf("rodada %d: o pedido não cresceu", rodada)
		}
		saldoAnterior = quitado.TotalCents
	}
}

// Um pagamento simples, sem desconto e cobrindo tudo: o ERP já tem essa parcela.
// Reescrever gastaria uma escrita do teto de 30/min para não mudar nada.
func TestPagamentoUnicoSemDescontoNaoGastaEscrita(t *testing.T) {
	svc, repo, erp := montarParcelas(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")
	repo.cobrar("cart-1", time.Now(), -1, "pix")
	_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)
	erp.escritas = 0

	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("recompondo: %v", err)
	}
	if split.Reescrito || erp.escritas != 0 {
		t.Errorf("gastou %d escrita(s) para não mudar nada", erp.escritas)
	}
}
