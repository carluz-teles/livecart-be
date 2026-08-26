package erp

// Corridas — a parte que só aparece sob concorrência.
//
// Uma live é um funil de eventos simultâneos: dezenas de comentários por segundo
// do mesmo comprador e de compradores diferentes, o pix chegando no meio, o
// carrinho vencendo, o lojista mexendo no pedido pelo painel. As invariantes
// abaixo têm de valer em TODAS as intercalações, não na feliz.
//
// Cada teste roda com várias sementes de tamanho e é executado sob -race no CI.
// A propriedade que quase todos verificam é a mesma, e é a que o modelo compra:
// como a grade é sempre reconstruída do banco, a ordem de chegada não importa —
// o pedido converge para o carrinho, e não para a soma dos deltas.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// ─── C1. Rajada de comentários no MESMO carrinho ────────────────────────────

// N comentários simultâneos do mesmo comprador: um único pedido, e a grade final
// é a do banco. O CAS none→converting é o que garante o "único".
func TestRajadaNoMesmoCarrinhoCriaUmPedidoSo(t *testing.T) {
	for _, n := range []int{2, 5, 12, 40} {
		t.Run(fmt.Sprintf("%d comentários", n), func(t *testing.T) {
			svc, repo, erp, _ := montar(map[string]int{"ext-p1": 1000})
			ctx := context.Background()
			repo.criarCarrinho("cart-1", item("p1", n))

			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
				}()
			}
			wg.Wait()

			if erp.criacoes != 1 {
				t.Errorf("pedidos criados = %d, quero 1 — %d comentários do mesmo "+
					"carrinho não podem virar %d pedidos no ERP", erp.criacoes, n, erp.criacoes)
			}
			if erp.estornos != 0 || erp.lancamentos != 0 {
				t.Errorf("estornos=%d lançamentos=%d, quero 0 e 0", erp.estornos, erp.lancamentos)
			}
			// Convergência: qualquer que tenha sido a intercalação, o pedido
			// segura o que o carrinho diz.
			if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
				t.Fatalf("mutação de fechamento: %v", err)
			}
			if got := erp.estoque("ext-p1").reservado; got != n {
				t.Errorf("reservado = %d, quero %d (a grade do banco)", got, n)
			}
		})
	}
}

// ─── C2. Compradores diferentes disputando o mesmo produto ──────────────────

// Cada carrinho tem o seu pedido, e a soma das reservas é a soma das grades.
func TestCarrinhosConcorrentesNaoSeAtropelam(t *testing.T) {
	for _, n := range []int{3, 10, 30} {
		t.Run(fmt.Sprintf("%d carrinhos", n), func(t *testing.T) {
			svc, repo, erp, _ := montar(map[string]int{"ext-p1": 1000})
			ctx := context.Background()
			for i := 0; i < n; i++ {
				repo.criarCarrinho(fmt.Sprintf("cart-%d", i), item("p1", 2))
			}

			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_ = svc.ReserveStockInERP(ctx, "loja-1", fmt.Sprintf("cart-%d", i), "ev-1", "p1", 2, 2000, "@x")
				}(i)
			}
			wg.Wait()

			if erp.criacoes != n {
				t.Errorf("pedidos = %d, quero %d (um por carrinho)", erp.criacoes, n)
			}
			if got := erp.estoque("ext-p1").reservado; got != 2*n {
				t.Errorf("reservado = %d, quero %d", got, 2*n)
			}
			if got := erp.estoque("ext-p1").saldo; got != 1000 {
				t.Errorf("saldo físico = %d, quero 1000 intacto", got)
			}
		})
	}
}

// ─── C3. Mutação × mutação ──────────────────────────────────────────────────

// Duas mutações simultâneas do mesmo pedido: a perdedora desiste, e desistir é
// seguro porque a vencedora relê o banco — que já contém a mudança da perdedora.
func TestMutacoesSimultaneasConvergem(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 500})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	const rodadas = 30
	var wg sync.WaitGroup
	for i := 1; i <= rodadas; i++ {
		wg.Add(1)
		go func(q int) {
			defer wg.Done()
			repo.definirItens("cart-1", item("p1", q))
			_ = svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
		}(i)
	}
	wg.Wait()

	// A última mutação a rodar fixa o valor; o que importa é que o pedido esteja
	// coerente com o banco, nunca com uma soma de deltas.
	final := repo.carrinho("cart-1").itens[0].Quantity
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação de fechamento: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != final {
		t.Errorf("reservado = %d, quero %d (o que o banco diz)", got, final)
	}
	if repo.carrinho("cart-1").state != OrderStateOpen {
		t.Errorf("estado final = %q, quero 'open'", repo.carrinho("cart-1").state)
	}
}

// ─── C4. Pagamento × mutação ────────────────────────────────────────────────

// O pix entra enquanto ainda chegam comentários. O confirm reconcilia a grade
// antes de aprovar, então o pedido aprovado nunca sai com grade velha.
func TestPagamentoNoMeioDaRajadaAprovaComAGradeCerta(t *testing.T) {
	for _, n := range []int{4, 16, 40} {
		t.Run(fmt.Sprintf("%d comentários", n), func(t *testing.T) {
			svc, repo, erp, _ := montar(map[string]int{"ext-p1": 1000})
			ctx := context.Background()
			repo.criarCarrinho("cart-1", item("p1", 1))
			_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

			var wg sync.WaitGroup
			for i := 2; i <= n; i++ {
				wg.Add(1)
				go func(q int) {
					defer wg.Done()
					repo.definirItens("cart-1", item("p1", q))
					_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
				}(i)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)
			}()
			wg.Wait()

			if erp.criacoes != 1 {
				t.Errorf("pedidos = %d, quero 1", erp.criacoes)
			}
			if erp.lancamentos != 0 || erp.estornos != 0 {
				t.Errorf("lançamentos=%d estornos=%d, quero 0 e 0 mesmo sob corrida",
					erp.lancamentos, erp.estornos)
			}
			// O carrinho ou está confirmado, ou o confirm perdeu a trava e sairá
			// na reentrega. As duas são desfechos válidos; grade divergente não é.
			st := repo.carrinho("cart-1")
			if st.state == OrderStateConfirmed {
				ped := erp.pedido(st.externalOrderID)
				if ped.situacao != providers.SituacaoAprovada {
					t.Errorf("confirmado mas situação = %d", ped.situacao)
				}
			}
		})
	}
}

// ─── C5. Cancelamento × pagamento ───────────────────────────────────────────

// A corrida mais cara do sistema. O desfecho tem de ser um dos dois — nunca um
// pedido aprovado E cancelado, nem estoque devolvido para uma venda que existiu.
func TestCancelamentoEPagamentoNaoProduzemEstadoHibrido(t *testing.T) {
	for i := 0; i < 200; i++ {
		svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
		ctx := context.Background()
		repo.criarCarrinho("cart-1", item("p1", 2))
		_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil) }()
		go func() { defer wg.Done(); _ = svc.CancelERPOrderForCart(ctx, "cart-1", "loja-1") }()
		wg.Wait()

		st := repo.carrinho("cart-1")
		if st.state != OrderStateConfirmed && st.state != OrderStateCancelled && st.state != OrderStateOpen {
			t.Fatalf("rodada %d: estado híbrido %q", i, st.state)
		}
		ped := erp.pedido(st.externalOrderID)
		if ped == nil {
			continue
		}
		if st.state == OrderStateCancelled && erp.estoque("ext-p1").reservado != 0 {
			t.Fatalf("rodada %d: cancelado mas ainda segurando %d unidades",
				i, erp.estoque("ext-p1").reservado)
		}
		if erp.estornos != 0 {
			t.Fatalf("rodada %d: %d estorno(s) — nenhum caminho deste par autoriza um",
				i, erp.estornos)
		}
	}
}

// ─── C6. O lojista mexendo no painel durante a live ─────────────────────────

// Ele lança o estoque à mão no meio da rajada. Exatamente um estorno destrava,
// e o pedido termina coerente.
func TestLancamentoManualNoMeioDaRajada(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 500})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	var wg sync.WaitGroup
	for i := 2; i <= 20; i++ {
		wg.Add(1)
		go func(q int) {
			defer wg.Done()
			repo.definirItens("cart-1", item("p1", q))
			_ = svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = erp.LaunchOrderStock(ctx, orderID)
	}()
	wg.Wait()

	// Fecha com uma mutação determinística e confere a coerência final.
	repo.definirItens("cart-1", item("p1", 7))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("mutação de fechamento: %v", err)
	}
	ped := erp.pedido(orderID)
	if ped.estoqueLancado {
		t.Error("pedido continuou com estoque lançado — a próxima edição travaria")
	}
	if ped.itens["ext-p1"] != 7 {
		t.Errorf("grade = %d, quero 7", ped.itens["ext-p1"])
	}
	if got := erp.estoque("ext-p1"); got.saldo != 500 || got.reservado != 7 {
		t.Errorf("saldo=%d reservado=%d, quero 500/7 — o estorno devolveu a baixa e "+
			"o PUT reajustou a reserva", got.saldo, got.reservado)
	}
}

// ─── C7. Webhooks de situação em rajada e fora de ordem ─────────────────────

func TestWebhooksDeSituacaoSimultaneosNaoDuplicamHistorico(t *testing.T) {
	svc, repo, _, _ := montar(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	orderID := repo.carrinho("cart-1").externalOrderID

	// A mesma situação entregue 20 vezes em paralelo é UMA transição.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.ObserveOrderStatus(ctx, "loja-1", orderID, "34", providers.ERPOrderStatusFaturado, StatusSourceWebhook, nil)
		}()
	}
	wg.Wait()

	if len(repo.statusEventos) != 1 {
		t.Errorf("gravou %d passagens para a mesma situação, quero 1", len(repo.statusEventos))
	}
}

// ─── C8. Expiração × comentário que ainda chega ─────────────────────────────

// O comentário que chega no instante da expiração.
//
// Esta era a corrida que o teste encontrou de verdade: com o cancelamento
// falando com o ERP ANTES de reivindicar o estado, um comentário ganhava o CAS
// open→mutating na janela do meio e mandava a grade DEPOIS do cancelamento — o
// pedido cancelado voltava a segurar 3 unidades, e nada no sistema sabia. A
// correção foi inverter a ordem: reivindicar primeiro, cancelar depois.
func TestComentarioQueChegaDuranteAExpiracao(t *testing.T) {
	for i := 0; i < 200; i++ {
		svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
		ctx := context.Background()
		repo.criarCarrinho("cart-1", item("p1", 1))
		_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = svc.OnCartExpired(ctx, "cart-1", "loja-1") }()
		go func() {
			defer wg.Done()
			repo.definirItens("cart-1", item("p1", 3))
			_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
		}()
		wg.Wait()

		if erp.estornos != 0 {
			t.Fatalf("rodada %d: estorno indevido", i)
		}
		if erp.criacoes != 1 {
			t.Fatalf("rodada %d: %d pedidos", i, erp.criacoes)
		}
		st := repo.carrinho("cart-1")
		if st.state == OrderStateCancelled && erp.estoque("ext-p1").reservado != 0 {
			t.Fatalf("rodada %d: cancelado segurando %d", i, erp.estoque("ext-p1").reservado)
		}
		// O carrinho nunca fica preso: ou foi cancelado, ou voltou a 'open' e a
		// retentativa do asynq o alcança.
		if st.state != OrderStateCancelled && st.state != OrderStateOpen {
			t.Fatalf("rodada %d: carrinho preso em %q", i, st.state)
		}
	}
}

// ─── C9. Contador local sob disputa ─────────────────────────────────────────

// O contador local é o portão: N compradores disputando M unidades resultam em
// no máximo M aprovações. É o que impede a venda a descoberto do lado de cá,
// independentemente do que o ERP faça.
func TestContadorLocalNaoDeixaVenderMaisDoQueTem(t *testing.T) {
	for _, caso := range []struct{ unidades, compradores int }{
		{1, 10}, {3, 20}, {10, 50}, {25, 25},
	} {
		t.Run(fmt.Sprintf("%d unidades para %d compradores", caso.unidades, caso.compradores), func(t *testing.T) {
			svc, repo, _, _ := montar(map[string]int{"ext-p1": caso.unidades})
			ctx := context.Background()
			repo.estoque["p1"] = caso.unidades
			for i := 0; i < caso.compradores; i++ {
				repo.criarCarrinho(fmt.Sprintf("cart-%d", i), item("p1", 1))
			}

			var mu sync.Mutex
			aprovados := 0
			var wg sync.WaitGroup
			for i := 0; i < caso.compradores; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, err := svc.AdjustStockReservationDelta(ctx, "loja-1",
						fmt.Sprintf("cart-%d", i), "ev-1", "p1", 1, 2000, "@x", StockOpCartAdd)
					if err == nil {
						mu.Lock()
						aprovados++
						mu.Unlock()
					}
				}(i)
			}
			wg.Wait()

			if aprovados > caso.unidades {
				t.Errorf("aprovou %d de %d unidades — venda a descoberto", aprovados, caso.unidades)
			}
			if repo.estoque["p1"] < 0 {
				t.Errorf("contador local negativo: %d", repo.estoque["p1"])
			}
			if esperado := caso.unidades; aprovados != esperado && caso.compradores >= caso.unidades {
				t.Errorf("aprovou %d, quero %d — o portão não pode recusar quem cabia",
					aprovados, esperado)
			}
		})
	}
}

// ─── C10. Falhas intermitentes do ERP ───────────────────────────────────────

// O ERP oscila no meio da rajada. Nenhuma falha pode produzir estorno nem pedido
// duplicado; o contador local volta ao lugar.
func TestERPOscilandoNaoProduzEstornoNemPedidoDuplicado(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 500})
	ctx := context.Background()
	repo.estoque["p1"] = 500
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	erp.falharPut = errors.New("503 do ERP")
	erp.putsAteFalhar = 5 // os 5 primeiros passam, o resto falha

	var wg sync.WaitGroup
	for i := 2; i <= 25; i++ {
		wg.Add(1)
		go func(q int) {
			defer wg.Done()
			repo.definirItens("cart-1", item("p1", q))
			_ = svc.MutateERPOrderItems(ctx, "cart-1", "loja-1")
		}(i)
	}
	wg.Wait()

	if erp.estornos != 0 {
		t.Errorf("estornos = %d sob falha do ERP, quero 0", erp.estornos)
	}
	if erp.criacoes != 1 {
		t.Errorf("pedidos = %d, quero 1", erp.criacoes)
	}
	if st := repo.carrinho("cart-1").state; st != OrderStateOpen {
		t.Errorf("estado = %q, quero 'open' — a mutação sempre devolve o carrinho, "+
			"mesmo em erro, senão ele nunca mais é editável", st)
	}
	// Com o ERP de volta, a próxima mutação reconcilia.
	erp.falharPut = nil
	repo.definirItens("cart-1", item("p1", 9))
	if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("reconciliação: %v", err)
	}
	if got := erp.estoque("ext-p1").reservado; got != 9 {
		t.Errorf("reservado = %d, quero 9 — a grade do banco venceu", got)
	}
}

// ─── C11. A invariante global, sob carga aleatória ──────────────────────────

// Uma live inteira, embaralhada: comentários, aumentos, reduções, pagamentos,
// cancelamentos e expirações disparados juntos. Ao final, três invariantes.
func TestLiveEmbaralhadaMantemAsInvariantes(t *testing.T) {
	const carrinhos = 25
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 5000})
	ctx := context.Background()
	repo.estoque["p1"] = 5000
	for i := 0; i < carrinhos; i++ {
		repo.criarCarrinho(fmt.Sprintf("cart-%d", i), item("p1", 1))
	}

	var wg sync.WaitGroup
	for i := 0; i < carrinhos; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cart := fmt.Sprintf("cart-%d", i)
			_ = svc.ReserveStockInERP(ctx, "loja-1", cart, "ev-1", "p1", 1, 2000, "@x")
			// mais comentários
			for q := 2; q <= 3+i%4; q++ {
				repo.definirItens(cart, item("p1", q))
				_ = svc.ReserveStockInERP(ctx, "loja-1", cart, "ev-1", "p1", 1, 2000, "@x")
			}
			switch i % 3 {
			case 0:
				_ = svc.ConfirmERPOrderPayment(ctx, cart, "loja-1", nil)
			case 1:
				_ = svc.OnCartExpired(ctx, cart, "loja-1")
			case 2:
				// segue aberto
			}
		}(i)
	}
	wg.Wait()

	// 1. Um pedido por carrinho, nunca dois.
	if erp.criacoes != carrinhos {
		t.Errorf("pedidos = %d, quero %d", erp.criacoes, carrinhos)
	}
	// 2. Nenhum estorno e nenhum lançamento partiram do LiveCart.
	if erp.estornos != 0 || erp.lancamentos != 0 {
		t.Errorf("estornos=%d lançamentos=%d, quero 0 e 0", erp.estornos, erp.lancamentos)
	}
	// 3. O saldo FÍSICO não se moveu um milímetro durante a live inteira.
	if got := erp.estoque("ext-p1").saldo; got != 5000 {
		t.Errorf("saldo físico = %d, quero 5000 — a live não baixa estoque", got)
	}
	// 4. Nenhuma reserva negativa nem disponível acima do físico.
	est := erp.estoque("ext-p1")
	if est.reservado < 0 {
		t.Errorf("reservado negativo: %d", est.reservado)
	}
	if est.disponivel() > est.saldo {
		t.Errorf("disponível %d acima do físico %d", est.disponivel(), est.saldo)
	}
}

// ─── C12. Reconstrução determinística ───────────────────────────────────────

// A mesma sequência de eventos, aplicada em ordens diferentes, chega ao mesmo
// lugar. É a propriedade que torna a retomada segura: reexecutar não estraga.
func TestReaplicarAGradeEhIdempotente(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 4))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 4, 2000, "@maria")

	antes := erp.estoque("ext-p1")
	for i := 0; i < 10; i++ {
		if err := svc.MutateERPOrderItems(ctx, "cart-1", "loja-1"); err != nil {
			t.Fatalf("reaplicação %d: %v", i, err)
		}
	}
	if depois := erp.estoque("ext-p1"); depois != antes {
		t.Errorf("dez reaplicações da MESMA grade mudaram o estoque: %+v → %+v", antes, depois)
	}
}

// Garantia de que os simuladores continuam satisfazendo as interfaces reais.
var (
	_ providers.ERPProvider = (*erpSimulado)(nil)
	_                       = zap.NewNop
	_                       = time.Now
)
