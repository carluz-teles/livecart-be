package erp

// Fundir carrinhos é fundir os pedidos.
//
// O que estes testes protegem não é o resultado final — é a ORDEM. O destino
// cresce antes de a origem ser solta, porque o intervalo entre as duas coisas é
// exatamente o tempo em que outra compradora poderia levar a peça.

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

// erpEspiaOrdem registra a sequência de escritas, que é o que se quer provar.
type erpEspiaOrdem struct {
	*erpComParcelas
	sequencia        []string
	falharAoCancelar map[string]error
	// reservadoNoCancelamento guarda quanto o produto estava reservado no
	// instante em que cada cancelamento aconteceu.
	reservadoNoCancelamento []int
	produtoObservado        string
}

// cancelou diz se o ERP chegou a receber o cancelamento daquele pedido. Ler a
// sequência é o que separa "o relatório disse que soltou" de "o pedido saiu
// mesmo de lá" — e é a diferença entre peça devolvida e peça contada duas vezes.
func (e *erpEspiaOrdem) cancelou(orderID string) bool {
	for _, p := range e.sequencia {
		if p == "cancelou:"+orderID {
			return true
		}
	}
	return false
}

func (e *erpEspiaOrdem) UpdateOrderItems(ctx context.Context, orderID string, itens []providers.ERPOrderItem) error {
	e.sequencia = append(e.sequencia, "cresceu:"+orderID)
	return e.erpComParcelas.UpdateOrderItems(ctx, orderID, itens)
}

// O cancelamento do pedido é uma transição de situação — é assim que o ERP o
// expõe, e é o que devolve a reserva sozinho.
func (e *erpEspiaOrdem) SetOrderSituacao(ctx context.Context, orderID string, situacao int) error {
	if situacao != providers.SituacaoCancelada {
		return e.erpComParcelas.SetOrderSituacao(ctx, orderID, situacao)
	}
	e.sequencia = append(e.sequencia, "cancelou:"+orderID)
	if e.produtoObservado != "" {
		e.reservadoNoCancelamento = append(e.reservadoNoCancelamento,
			e.estoque(e.produtoObservado).reservado)
	}
	if err, ok := e.falharAoCancelar[orderID]; ok && err != nil {
		return err
	}
	return e.erpComParcelas.SetOrderSituacao(ctx, orderID, situacao)
}

func montarFusao(saldos map[string]int) (*Service, *repoSimulado, *erpEspiaOrdem) {
	e := novoERPSimulado(saldos)
	r := novoRepoSimulado()
	espia := &erpEspiaOrdem{erpComParcelas: &erpComParcelas{erpSimulado: e}}
	c := &colabSimulado{erp: espia, repo: r}
	s := NewService(r, c, zap.NewNop())
	s.SetOrderStatusRepository(r)
	s.SetWriteLimits(limitesAbertos())
	return s, r, espia
}

// doisCarrinhosDeEventosDiferentes encena o comprador antes da promoção: um
// carrinho por evento, cada um com seu pedido segurando peça no ERP.
func doisCarrinhosDeEventosDiferentes(t *testing.T, svc *Service, repo *repoSimulado) (dest, origem string) {
	t.Helper()
	ctx := context.Background()
	repo.criarCarrinho("cart-segunda", item("p1", 2))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-segunda", "ev-1", "p1", 2, 2000, "@maria"); err != nil {
		t.Fatalf("carrinho de segunda: %v", err)
	}
	repo.criarCarrinho("cart-quinta", item("p1", 3))
	if err := svc.ReserveStockInERP(ctx, "loja-1", "cart-quinta", "ev-2", "p1", 3, 2000, "@maria"); err != nil {
		t.Fatalf("carrinho de quinta: %v", err)
	}
	return "cart-quinta", "cart-segunda" // o mais recente é o eterno
}

// fundir encena o que a consolidação faz no banco antes de chamar o ERP: os
// itens já mudaram de dono e o carrinho de origem já está vazio e fechado.
func fundir(repo *repoSimulado, dest, origem string) []ERPOrderMerge {
	itens := repo.carrinho(origem).itens
	for _, it := range itens {
		repo.acrescentarItem(dest, it)
	}
	repo.esvaziarCarrinho(origem)
	return []ERPOrderMerge{{
		SourceCartID:    origem,
		ExternalOrderID: repo.carrinho(origem).externalOrderID,
	}}
}

// ─── A ordem ────────────────────────────────────────────────────────────────

// O destino cresce ANTES de a origem ser solta. Invertido, existe um instante
// em que ninguém segura as peças do comprador.
func TestFusaoCresceODestinoAntesDeSoltarAOrigem(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	orfaos := fundir(repo, dest, origem)
	erp.sequencia = nil

	if _, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos); err != nil {
		t.Fatalf("fusão: %v", err)
	}

	var cresceu, cancelou = -1, -1
	for i, passo := range erp.sequencia {
		if cresceu < 0 && len(passo) > 8 && passo[:8] == "cresceu:" {
			cresceu = i
		}
		if cancelou < 0 && len(passo) > 9 && passo[:9] == "cancelou:" {
			cancelou = i
		}
	}
	if cresceu < 0 || cancelou < 0 {
		t.Fatalf("faltou passo na sequência: %v", erp.sequencia)
	}
	if cresceu > cancelou {
		t.Errorf("cancelou antes de crescer (%v) — nesse intervalo as peças do "+
			"comprador ficam sem dono e outra compradora as leva", erp.sequencia)
	}
}

// No instante do cancelamento as unidades já estão reservadas nos DOIS pedidos.
// É esse excesso momentâneo que garante que ninguém fica descoberto.
func TestNoInstanteDoCancelamentoAPecaJaEstaNoPedidoNovo(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	erp.produtoObservado = "ext-p1"
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	orfaos := fundir(repo, dest, origem)

	if _, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos); err != nil {
		t.Fatalf("fusão: %v", err)
	}
	if len(erp.reservadoNoCancelamento) != 1 {
		t.Fatalf("cancelamentos observados = %d, quero 1", len(erp.reservadoNoCancelamento))
	}
	// 2 (origem) + 5 (destino, já somado) = 7 no instante em que se cancela.
	if got := erp.reservadoNoCancelamento[0]; got != 7 {
		t.Errorf("reservado no instante do cancelamento = %d, quero 7 — as 5 un. "+
			"do pedido novo mais as 2 que o antigo ainda segurava; menos que isso "+
			"significa que alguma peça ficou descoberta", got)
	}
	if fim := erp.estoque("ext-p1").reservado; fim != 5 {
		t.Errorf("reservado no fim = %d, quero 5 — o excesso momentâneo tem de "+
			"voltar quando o pedido antigo é cancelado", fim)
	}
}

// O resultado: um pedido só, com a soma.
func TestFusaoDeixaUmPedidoSoComASoma(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	pedidoAntigo := repo.carrinho(origem).externalOrderID
	orfaos := fundir(repo, dest, origem)

	rel, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos)
	if err != nil {
		t.Fatalf("fusão: %v", err)
	}
	if len(rel.Released) != 1 || len(rel.Stuck) != 0 {
		t.Errorf("soltos=%v presos=%v, quero 1 e 0", rel.Released, rel.Stuck)
	}
	pedidoNovo := repo.carrinho(dest).externalOrderID
	if q := erp.quantidadeNoPedido(pedidoNovo, "ext-p1"); q != 5 {
		t.Errorf("o pedido eterno tem %d un., quero 5 (2 de segunda + 3 de quinta)", q)
	}
	if q := erp.quantidadeNoPedido(pedidoAntigo, "ext-p1"); q != 0 {
		t.Errorf("o pedido antigo ainda tem %d un.", q)
	}
	if est := repo.carrinho(origem).state; est != OrderStateCancelled {
		t.Errorf("o carrinho fundido ficou em %q, quero %q", est, OrderStateCancelled)
	}
}

// ─── Quando dá errado ───────────────────────────────────────────────────────

// Se o destino não consegue crescer, NADA é solto. O estado seguro é o antigo:
// os pedidos velhos continuam segurando as peças do comprador.
func TestDestinoQueNaoCresceNaoSoltaNinguem(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	pedidoAntigo := repo.carrinho(origem).externalOrderID
	orfaos := fundir(repo, dest, origem)
	repo.definirStatusERP(dest, string(providers.ERPOrderStatusFaturado))

	rel, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos)
	if err == nil {
		t.Fatal("não avisou que o destino não pôde crescer")
	}
	if len(rel.Released) != 0 {
		t.Errorf("soltou %v mesmo sem o destino ter crescido — as peças do "+
			"comprador ficariam sem reserva nenhuma", rel.Released)
	}
	if q := erp.quantidadeNoPedido(pedidoAntigo, "ext-p1"); q != 2 {
		t.Errorf("o pedido antigo tem %d un., quero 2 intactas", q)
	}
}

// Um cancelamento que falha não interrompe os outros, e o que ficou preso sobe
// como erro — é a mesma peça contada em dois pedidos até alguém mexer.
func TestPedidoQueNaoSoltaSobeComoErroSemInterromperOsOutros(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100, "ext-p2": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-a", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-a", "ev-1", "p1", 1, 2000, "@maria")
	repo.criarCarrinho("cart-b", item("p2", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-b", "ev-2", "p2", 1, 2000, "@maria")
	repo.criarCarrinho("cart-eterno", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-eterno", "ev-3", "p1", 1, 2000, "@maria")

	orfaos := append(fundir(repo, "cart-eterno", "cart-a"), fundir(repo, "cart-eterno", "cart-b")...)
	erp.falharAoCancelar = map[string]error{orfaos[0].ExternalOrderID: fmt.Errorf("503 do ERP")}

	rel, err := svc.MergeERPOrdersIntoCart(ctx, "cart-eterno", "loja-1", orfaos)
	if err == nil {
		t.Error("engoliu o pedido que ficou segurando peça")
	}
	if len(rel.Stuck) != 1 {
		t.Errorf("presos = %v, quero exatamente 1", rel.Stuck)
	}
	if len(rel.Released) != 1 {
		t.Errorf("soltos = %v — a falha de um não pode impedir o outro de ser "+
			"solto; desistir no primeiro erro deixaria peça presa em silêncio",
			rel.Released)
	}
}

// Promoção sem nada a fundir é uma passagem limpa: nenhuma escrita no ERP.
func TestFusaoSemOrfaosNaoEscreveNoERP(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	erp.sequencia = nil

	rel, err := svc.MergeERPOrdersIntoCart(ctx, "cart-1", "loja-1", nil)
	if err != nil {
		t.Fatalf("fusão vazia: %v", err)
	}
	if len(rel.Released) != 0 || len(rel.Stuck) != 0 {
		t.Errorf("relatório não vazio: %+v", rel)
	}
	if len(erp.sequencia) != 0 {
		t.Errorf("gastou escrita à toa: %v — o teto da conta é 30 por minuto", erp.sequencia)
	}
}

// ─── Como a origem é solta ──────────────────────────────────────────────────
//
// O cancelamento comum não sai de 'confirmed', e essa guarda existe para impedir
// estorno acidental. Mas ela não pode travar a fusão de um pedido confirmado que
// não tem cobrança nenhuma em cima: aí não há estorno a acontecer, e recusar
// deixa o pedido vivo segurando peça que já está no pedido do anfitrião — a
// mesma unidade contada duas vezes.
//
// Foi o caso real de staging em 28/08: o carrinho 2a965cca estava 'confirmed'
// com payment_status 'pending' e zero linhas no livro, e a junção do painel não
// tinha saída.

func confirmarPedido(t *testing.T, repo *repoSimulado, cartID string) {
	t.Helper()
	ok, err := repo.TransitionCartERPOrderState(context.Background(), cartID, OrderStateOpen, OrderStateConfirmed)
	if err != nil || !ok {
		t.Fatalf("não consegui deixar %s confirmado: ok=%v err=%v", cartID, ok, err)
	}
}

func TestOrigemConfirmadaSemDinheiroESolta(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	confirmarPedido(t, repo, origem)

	orfaos := fundir(repo, dest, origem)
	orfaos[0].SemDinheiro = true // quem chamou leu o livro ANTES do vínculo

	rel, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos)
	if err != nil {
		t.Fatalf("a fusão devia soltar o pedido confirmado sem cobrança: %v", err)
	}
	if len(rel.Stuck) > 0 {
		t.Fatalf("pedido preso: %v — ele continua segurando peça que já está no do anfitrião", rel.Stuck)
	}
	if len(rel.Released) != 1 {
		t.Fatalf("solto = %v, queria exatamente o pedido da origem", rel.Released)
	}
	if !erp.cancelou(rel.Released[0]) {
		t.Errorf("o pedido saiu do relatório como solto mas o ERP nunca recebeu o cancelamento")
	}
}

// A afirmação é o que libera. Sem ela a origem vai pelo caminho comum, que
// recusa 'confirmed' — e é assim que o pedido PAGO fica protegido, já que quem
// chama só afirma o que leu no livro de pagamentos.
func TestOrigemConfirmadaSemAfirmacaoNaoESolta(t *testing.T) {
	svc, repo, erp := montarFusao(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	dest, origem := doisCarrinhosDeEventosDiferentes(t, svc, repo)
	confirmarPedido(t, repo, origem)

	orfaos := fundir(repo, dest, origem) // SemDinheiro fica falso — o padrão
	rel, err := svc.MergeERPOrdersIntoCart(ctx, dest, "loja-1", orfaos)
	if err == nil {
		t.Fatal("devia subir como erro: o pedido continua segurando peça depois da fusão")
	}
	if len(rel.Released) > 0 {
		t.Fatalf("soltou %v sem a afirmação de que não há dinheiro — é um estorno silencioso", rel.Released)
	}
	if erp.cancelou(repo.carrinho(origem).externalOrderID) {
		t.Error("o ERP recebeu o cancelamento de um pedido que podia ter pagamento")
	}
}
