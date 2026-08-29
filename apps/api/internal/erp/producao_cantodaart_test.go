package erp

// Os pedidos que falharam de verdade na cantodaart, replicados.
//
// Quatro pedidos ficaram com a finalização do ERP em `failed` nos três dias
// anteriores a 26/08/2026. Os erros vieram do banco de PRODUÇÃO, lidos na íntegra
// e citados aqui como estão. Três deles são impossíveis no modelo novo porque a
// operação que falhava deixou de existir; o quarto virou retomável.
//
// Cada teste abaixo é a prova executável disso. Não são cenários inventados: são
// aqueles pedidos, com a mesma sequência que os produziu.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// ─── #1098 e #1186 · "relançamento de estoque do pedido falhou" ─────────────

// Erro em produção, pedido #1098 (25/08 22:08), 4 tentativas:
//
//	relançamento de estoque do pedido 848003866 falhou após fallback:
//	launch stock failed: status 400, message: Não é possível integrar o estoque
//	deste pedido pois o saldo em estoque de um ou mais produtos é insuficiente.
//
// E o #1186 (23/08 23:10), mesma frase, pedido 847911430.
//
// O ciclo antigo era estornar → PUT /itens → LANÇAR, e o lançamento final é onde
// os dois morreram: o saldo já não comportava a baixa. O pedido ficava editado,
// sem estoque lançado, e a venda parada.
//
// No modelo novo o LiveCart não lança estoque em caminho nenhum. Este teste
// percorre a mesma sequência — pedido criado, grade mutada várias vezes, venda
// paga — e exige zero lançamentos.
func TestProducao1098_RelancamentoDeEstoqueNaoExisteMais(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 3})
	ctx := context.Background()
	repo.criarCarrinho("cart-1098", item("p1", 1))

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1098", "ev-1", "p1", 1, 2000, "@cliente"); err != nil {
		t.Fatalf("1º comentário: %v", err)
	}
	// A compradora volta várias vezes, como no pedido real.
	for _, qtd := range []int{2, 3, 5, 8} {
		repo.definirItens("cart-1098", item("p1", qtd))
		if err := svc.MutateERPOrderItems(ctx, "cart-1098", "loja-1"); err != nil {
			t.Fatalf("mutação para %d un.: %v", qtd, err)
		}
	}
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1098", "loja-1", nil); err != nil {
		t.Fatalf("pagamento: %v", err)
	}

	if erp.lancamentos != 0 {
		t.Errorf("houve %d lançamento(s) de estoque — é a chamada que derrubou o "+
			"#1098 e o #1186 em produção, e ela não deve existir em caminho nenhum",
			erp.lancamentos)
	}
	if erp.estornos != 0 {
		t.Errorf("houve %d estorno(s) — o ciclo estornar→PUT→lançar saiu junto", erp.estornos)
	}
	// A venda fechou mesmo com o saldo (3) abaixo do pedido (8): o ERP não trava,
	// e quem decide o que pode ser vendido é o contador local, antes daqui.
	if repo.carrinho("cart-1098").state != OrderStateConfirmed {
		t.Errorf("a venda não fechou: estado %q", repo.carrinho("cart-1098").state)
	}
}

// ─── #1115 · "unresolved ERP stock movement: unconfirmed" ───────────────────

// Erro em produção, pedido #1115 (25/08 13:50), 3 tentativas:
//
//	cart 141c113f-6a60-4c4a-89b8-a4307c94b193 has 1 unresolved ERP stock
//	movement(s): [unconfirmed [b886495c-0d2f-…]]
//
// `unconfirmed` era o desfecho de um movimento manual cujo destino ninguém
// sabia: a chamada saiu, a resposta não voltou, e repetir criaria um segundo
// lançamento. O carrinho pago ficava travado esperando decisão humana — foi 68%
// das reservas numa live simulada.
//
// O estado não existe porque o movimento não existe. A prova é estrutural: o
// carrinho paga e fecha sem que exista uma única linha de reserva manual.
func TestProducao1115_MovimentoNaoResolvidoNaoExisteMais(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 50})
	ctx := context.Background()
	repo.criarCarrinho("cart-1115", item("p1", 4))

	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1115", "ev-1", "p1", 4, 2000, "@cliente"); err != nil {
		t.Fatalf("comentário: %v", err)
	}
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1115", "loja-1", nil); err != nil {
		t.Fatalf("pagamento: %v", err)
	}

	// O que segura a peça é o pedido, e ele é UM, identificado e consultável.
	orderID := repo.carrinho("cart-1115").externalOrderID
	if orderID == "" {
		t.Fatal("o carrinho pago não tem pedido")
	}
	if ped := erp.pedido(orderID); ped.itens["ext-p1"] != 4 {
		t.Errorf("grade do pedido = %d, quero 4", ped.itens["ext-p1"])
	}
	// Nenhuma chamada de destino ambíguo aconteceu: as únicas escritas foram
	// criar o pedido e movê-lo de situação, e as duas são consultáveis depois.
	if erp.lancamentos != 0 || erp.estornos != 0 {
		t.Errorf("lançamentos=%d estornos=%d — as duas chamadas cujo desfecho podia "+
			"ficar em dúvida", erp.lancamentos, erp.estornos)
	}
}

// ─── #1087 · "create order failed: status 429" ──────────────────────────────

// Erro em produção, pedido #1087 (25/08 12:15), 3 tentativas:
//
//	creating ERP order: create order failed: status 429:
//
// Este é o único dos quatro que o modelo novo NÃO torna impossível: criar o
// pedido continua sendo uma escrita, e o teto da conta continua sendo 30 por
// minuto. O que mudou é o desfecho — antes o carrinho ficava sem pedido e sem
// caminho de volta; agora o estado 'converting' é retomável, e a retomada procura
// o pedido antes de criar outro.
//
// O teste roda a falha e depois a retomada, exigindo UM pedido no fim.
func TestProducao1087_ErroNaCriacaoAgoraEhRetomavel(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1087", item("p1", 2))

	erp.falharCriacao = errors.New("create order failed: status 429: ")
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1087", "ev-1", "p1", 2, 2000, "@cliente"); err == nil {
		t.Fatal("a criação devia falhar, como em produção")
	}
	if got := repo.carrinho("cart-1087").state; got != OrderStateConverting {
		t.Fatalf("estado = %q, quero 'converting' — regredir para 'none' reabriria o "+
			"caminho e criaria um segundo pedido", got)
	}

	// O ERP volta. Passada a carência, o próximo comentário da mesma compradora
	// retoma — é o que separa "em voo agora" de "morreu no caminho".
	erp.falharCriacao = nil
	repo.idadeDaOperacao = 2 * time.Minute
	repo.definirItens("cart-1087", item("p1", 3))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-1087", "ev-1", "p1", 1, 2000, "@cliente"); err != nil {
		t.Fatalf("retomada: %v", err)
	}

	if erp.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1", erp.criacoes)
	}
	if got := repo.carrinho("cart-1087").state; got != OrderStateOpen {
		t.Errorf("estado = %q, quero 'open'", got)
	}
	if got := erp.estoque("ext-p1").reservado; got != 3 {
		t.Errorf("reservado = %d, quero 3 — a retomada tem de aplicar a grade ATUAL, "+
			"não a de quando falhou", got)
	}
}

// E se nem o comentário seguinte vier, a varredura resolve — que é o caso do
// #1087 real, onde a compradora comentou uma vez só.
func TestProducao1087_SemComentarioSeguinteAVarreduraResolve(t *testing.T) {
	svc, repo, erp, _ := montar(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1087", item("p1", 2))

	erp.falharCriacao = errors.New("create order failed: status 429: ")
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1087", "ev-1", "p1", 2, 2000, "@cliente")
	erp.falharCriacao = nil
	repo.idadeDaOperacao = 5 * time.Minute

	repo.presos = []StuckERPOrderOp{{CartID: "cart-1087", State: OrderStateConverting, StoreID: "loja-1"}}
	svc.RunERPOrderOpsSweep(ctx)

	if erp.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1 — a varredura passou a CRIAR, não só "+
			"adotar; antes ela desistia e o carrinho ficava sem pedido para sempre",
			erp.criacoes)
	}
	if got := repo.carrinho("cart-1087").state; got != OrderStateOpen {
		t.Errorf("estado = %q, quero 'open'", got)
	}
}

// ─── O ERP vendendo em paralelo ─────────────────────────────────────────────

// O lojista vende no balcão enquanto a live acontece. O ERP não trava: ele
// aceita o pedido e deixa o disponível negativo. Quem impede a venda a descoberto
// é o contador local, e ele precisa segurar mesmo com o saldo remoto caindo por
// baixo dos pés.
func TestERPVendendoEmParaleloNaoProduzOversellLocal(t *testing.T) {
	for _, caso := range []struct{ unidades, live, externas int }{
		{10, 10, 4}, {5, 12, 3}, {20, 25, 8}, {1, 6, 1},
	} {
		t.Run(fmt.Sprintf("%dun_%dlive_%dexternas", caso.unidades, caso.live, caso.externas), func(t *testing.T) {
			svc, repo, erp, _ := montar(map[string]int{"ext-p1": caso.unidades})
			ctx := context.Background()
			repo.estoque["p1"] = caso.unidades
			for i := 0; i < caso.live; i++ {
				repo.criarCarrinho(fmt.Sprintf("cart-%d", i), item("p1", 1))
			}

			// As vendas externas acontecem DURANTE, não antes.
			pronto := make(chan struct{})
			go func() {
				<-pronto
				for i := 0; i < caso.externas; i++ {
					_, _ = erp.CreateOrder(ctx, providers.ERPOrder{
						ExternalID: fmt.Sprintf("balcao-%d", i), ContactID: "c",
						Items: []providers.ERPOrderItem{{ProductID: "ext-p1", Quantity: 1, UnitPrice: 2000}},
					})
				}
			}()

			admitidos := 0
			close(pronto)
			for i := 0; i < caso.live; i++ {
				_, err := svc.AdjustStockReservationDelta(ctx, "loja-1",
					fmt.Sprintf("cart-%d", i), "ev-1", "p1", 1, 2000, "@x", StockOpCartAdd)
				if err == nil {
					admitidos++
				}
			}

			if admitidos > caso.unidades {
				t.Errorf("a live admitiu %d de %d unidades — venda a descoberto do "+
					"nosso lado", admitidos, caso.unidades)
			}
			if repo.estoque["p1"] < 0 {
				t.Errorf("contador local negativo: %d", repo.estoque["p1"])
			}
			// O ERP pode ficar negativo — ele não trava, e a venda externa é real.
			// O que não pode é o LiveCart ter contribuído para isso além do que o
			// contador local autorizou.
			if got := erp.estoque("ext-p1").saldo; got != caso.unidades {
				t.Errorf("saldo físico = %d, quero %d — nem a live nem o balcão "+
					"lançam estoque", got, caso.unidades)
			}
		})
	}
}

// E o espelho, quando o disponível cai por venda externa, PUXA o contador local
// para baixo — nunca para cima.
func TestEspelhoNuncaSobeOContadorLocal(t *testing.T) {
	// O disponível no ERP caiu para 3 (o balcão vendeu), mas o contador local
	// ainda diz 10 porque o webhook não chegou. Quando chegar, tem de descer.
	//
	// A regra vive no provider (só `disponivel` é lido) e no espelho; aqui o que
	// se fixa é a direção: um espelho que SOBE o portão reoferta peça vendida.
	_, repo, erp, _ := montar(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.estoque["p1"] = 10
	repo.criarCarrinho("cart-1", item("p1", 1))

	// Sete unidades saem pelo balcão.
	for i := 0; i < 7; i++ {
		_, _ = erp.CreateOrder(ctx, providers.ERPOrder{
			ExternalID: fmt.Sprintf("balcao-%d", i), ContactID: "c",
			Items: []providers.ERPOrderItem{{ProductID: "ext-p1", Quantity: 1, UnitPrice: 2000}},
		})
	}
	if got := erp.estoque("ext-p1").disponivel(); got != 3 {
		t.Fatalf("preparo: disponivel = %d, quero 3", got)
	}

	// A live segue pedindo. O contador local ainda está em 10 e vai admitir até
	// 10 — é o intervalo em que o espelho ainda não chegou, e ele existe.
	// O que o teste fixa é que o ERP nunca é consultado pelo saldo FÍSICO, que
	// continua 10 e reofertaria as sete vendidas.
	if got, err := erp.GetProductStock(ctx, "ext-p1"); err != nil || got != 3 {
		t.Errorf("GetProductStock = %d (err=%v), quero 3 — o físico (10) conta as "+
			"sete unidades que já têm dono", got, err)
	}
}
