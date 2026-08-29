package erp

// A drenagem é a operação mais perigosa deste sistema: ela mexe no estoque de uma
// loja com live no ar. O que os testes abaixo travam não é "funciona" — é a
// ORDEM, porque invertê-la solta a peça no meio de uma venda.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// drenoSimulado guarda as linhas de reserva manual e registra a sequência exata
// de eventos — é a sequência que os testes afirmam.
type drenoSimulado struct {
	mu           sync.Mutex
	carrinhos    []CartWithLegacyReservations
	linhas       map[string][]LegacyReservationRow
	revertidas   map[string]bool
	claimadas    map[string]bool
	falharClaim  bool
	falharMarcar bool
	eventos      []string
}

func (d *drenoSimulado) registrar(e string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.eventos = append(d.eventos, e)
}

func (d *drenoSimulado) ListCartsWithActiveReservations(context.Context, string) ([]CartWithLegacyReservations, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]CartWithLegacyReservations, len(d.carrinhos))
	copy(out, d.carrinhos)
	return out, nil
}

func (d *drenoSimulado) ListLegacyReservationsByCart(_ context.Context, cartID string) ([]LegacyReservationRow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []LegacyReservationRow
	for _, l := range d.linhas[cartID] {
		if !d.revertidas[l.ID] && !d.claimadas[l.ID] {
			out = append(out, l)
		}
	}
	return out, nil
}

func (d *drenoSimulado) ClaimReservationForReversal(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.falharClaim {
		return false, errors.New("banco fora do ar")
	}
	if d.revertidas[id] || d.claimadas[id] {
		return false, nil
	}
	d.claimadas[id] = true
	return true, nil
}

func (d *drenoSimulado) ReverseReservationByID(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.falharMarcar {
		return errors.New("banco fora do ar")
	}
	d.revertidas[id] = true
	d.eventos = append(d.eventos, "marcou:"+id)
	return nil
}

func (d *drenoSimulado) RestoreReservationToActive(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.claimadas, id)
	d.eventos = append(d.eventos, "devolveu:"+id)
	return nil
}

// erpComEstornoLegado é o simulador do ERP mais a rota antiga de entrada.
type erpComEstornoLegado struct {
	*erpSimulado
	dreno       *drenoSimulado
	falharTipoE error
	entradas    int
}

func (e *erpComEstornoLegado) ReverseLegacyStockExit(_ context.Context, produtoID string, qtd int, _ string) (string, error) {
	e.mu.Lock()
	e.entradas++
	e.mu.Unlock()
	e.dreno.registrar(fmt.Sprintf("estornou:%s:%d", produtoID, qtd))
	if e.falharTipoE != nil {
		return "", e.falharTipoE
	}
	// A entrada devolve a peça ao saldo FÍSICO — é o que a saída manual havia
	// tirado.
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.produtos[produtoID]; ok {
		p.saldo += qtd
	}
	return "MOV-E", nil
}

func montarDrenagem(saldos map[string]int) (*Service, *repoSimulado, *erpComEstornoLegado, *drenoSimulado) {
	e := novoERPSimulado(saldos)
	r := novoRepoSimulado()
	d := &drenoSimulado{linhas: map[string][]LegacyReservationRow{}, revertidas: map[string]bool{}, claimadas: map[string]bool{}}
	legado := &erpComEstornoLegado{erpSimulado: e, dreno: d}
	c := &colabSimulado{erp: legado, repo: r}
	// O provider que a criação do pedido resolve tem de ser o MESMO objeto, senão
	// a sequência de eventos não é comparável.
	s := NewService(r, c, zap.NewNop())
	s.SetOrderStatusRepository(r)
	s.SetDrainRepository(d)
	s.SetWriteLimits(limitesAbertos())
	return s, r, legado, d
}

// ─── A ordem é a coisa toda ─────────────────────────────────────────────────

// O pedido assume a guarda ANTES de qualquer estorno. Invertido, existem alguns
// segundos em que a peça está livre — e no meio de uma live alguém a compra.
func TestDrenagemCriaOPedidoAntesDeEstornar(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	dreno.carrinhos = []CartWithLegacyReservations{{CartID: "cart-1", StoreID: "loja-1", Rows: 1, Units: 3}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{
		{ID: "r1", CartID: "cart-1", ProductID: "p1", ExternalProductID: "ext-p1", Quantity: 3},
	}
	// A saída manual já havia baixado o físico: 10 é o saldo COM ela aplicada.
	antesFisico := legado.estoque("ext-p1").saldo

	rel, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0)
	if err != nil {
		t.Fatalf("drenagem: %v", err)
	}

	if rel.OrdersCreated != 1 || rel.RowsReversed != 1 {
		t.Fatalf("relatório: pedidos=%d linhas=%d, quero 1 e 1", rel.OrdersCreated, rel.RowsReversed)
	}
	// A prova da ordem: quando o estorno aconteceu, o pedido JÁ existia.
	if repo.carrinho("cart-1").externalOrderID == "" {
		t.Fatal("o carrinho terminou sem pedido")
	}
	est := legado.estoque("ext-p1")
	if est.saldo != antesFisico+3 {
		t.Errorf("saldo físico = %d, quero %d — o estorno devolve ao físico o que a "+
			"saída manual tinha tirado", est.saldo, antesFisico+3)
	}
	if est.reservado != 3 {
		t.Errorf("reservado = %d, quero 3 — o pedido assumiu a guarda", est.reservado)
	}
	// E o que realmente importa para o comprador: o disponível não se mexeu.
	if got := est.disponivel(); got != antesFisico {
		t.Errorf("disponivel = %d, quero %d (inalterado) — a troca de guarda não "+
			"pode nem liberar nem consumir estoque", got, antesFisico)
	}
}

// Se o pedido NÃO nasce, nada é estornado. A peça continua segurada pelo modelo
// antigo, que é feio mas correto — soltar seria vender duas vezes.
func TestDrenagemNaoEstornaSeOPedidoNaoNasce(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	dreno.carrinhos = []CartWithLegacyReservations{{CartID: "cart-1", StoreID: "loja-1", Rows: 1, Units: 3}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{{ID: "r1", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 3}}
	legado.falharCriacao = errors.New("503 do ERP")

	rel, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0)
	if err != nil {
		t.Fatalf("drenagem: %v", err)
	}
	if legado.entradas != 0 {
		t.Errorf("estornou %d vez(es) sem ter criado o pedido — a peça ficou livre "+
			"no meio de uma live", legado.entradas)
	}
	if rel.RowsReversed != 0 || rel.Failed != 1 {
		t.Errorf("relatório: revertidas=%d falhas=%d, quero 0 e 1", rel.RowsReversed, rel.Failed)
	}
	if dreno.revertidas["r1"] {
		t.Error("marcou a linha como revertida sem ter estornado")
	}
}

// ─── Reexecutar é seguro ────────────────────────────────────────────────────

// A drenagem de 126 carrinhos leva quase meia hora e vai ser interrompida. Rodar
// de novo não pode estornar duas vezes — estorno duplo INVENTA estoque.
func TestDrenagemRepetidaNaoEstornaDuasVezes(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	dreno.carrinhos = []CartWithLegacyReservations{{CartID: "cart-1", StoreID: "loja-1", Rows: 2, Units: 4}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{
		{ID: "r1", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 2},
		{ID: "r2", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 2},
	}

	for i := 0; i < 4; i++ {
		if _, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0); err != nil {
			t.Fatalf("passada %d: %v", i, err)
		}
		// Depois da primeira, o carrinho já tem pedido — como a lista real diria.
		dreno.carrinhos[0].ExternalOrderID = repo.carrinho("cart-1").externalOrderID
	}

	if legado.entradas != 2 {
		t.Errorf("entradas no ERP = %d, quero 2 (uma por linha) — quatro passadas "+
			"não podem virar oito devoluções", legado.entradas)
	}
	if legado.criacoes != 1 {
		t.Errorf("pedidos criados = %d, quero 1", legado.criacoes)
	}
}

// Carrinho que já tem pedido pula direto para o estorno: a guarda trocou numa
// passada anterior que foi interrompida.
func TestDrenagemDeCarrinhoQueJaTemPedidoSoEstorna(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@x")
	criacoesAntes := legado.criacoes

	dreno.carrinhos = []CartWithLegacyReservations{{
		CartID: "cart-1", StoreID: "loja-1", Rows: 1, Units: 2,
		ExternalOrderID: repo.carrinho("cart-1").externalOrderID,
	}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{{ID: "r1", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 2}}

	if _, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0); err != nil {
		t.Fatalf("drenagem: %v", err)
	}
	if legado.criacoes != criacoesAntes {
		t.Errorf("criou pedido para carrinho que já tinha um (%d → %d)", criacoesAntes, legado.criacoes)
	}
	if legado.entradas != 1 {
		t.Errorf("entradas = %d, quero 1", legado.entradas)
	}
}

// ─── Falhas ─────────────────────────────────────────────────────────────────

// O ERP recusa a entrada: a linha volta para 'active' e a próxima passada tenta
// de novo. Errar para o lado de "estornou de menos" é a escolha certa — falta de
// estorno aparece no saldo, estorno a mais inventa estoque.
func TestDrenagemDevolveALinhaQuandoOERPRecusa(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	dreno.carrinhos = []CartWithLegacyReservations{{CartID: "cart-1", StoreID: "loja-1", Rows: 1, Units: 2}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{{ID: "r1", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 2}}
	legado.falharTipoE = errors.New("429 do ERP")

	rel, _ := svc.DrainLegacyReservations(ctx, "loja-1", false, 0)
	if rel.RowsReversed != 0 {
		t.Errorf("contou %d revertidas apesar da recusa", rel.RowsReversed)
	}
	if dreno.revertidas["r1"] {
		t.Error("marcou revertida uma linha que o ERP recusou")
	}
	if dreno.claimadas["r1"] {
		t.Error("a linha ficou reivindicada e nunca mais seria tentada")
	}

	// Com o ERP de volta, a próxima passada resolve.
	legado.falharTipoE = nil
	dreno.carrinhos[0].ExternalOrderID = repo.carrinho("cart-1").externalOrderID
	if _, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0); err != nil {
		t.Fatalf("segunda passada: %v", err)
	}
	if !dreno.revertidas["r1"] {
		t.Error("a segunda passada não estornou a linha que tinha voltado")
	}
}

// ─── Ensaio ─────────────────────────────────────────────────────────────────

// O ensaio percorre a mesma lista e não escreve NADA. É como se mede o tamanho
// do trabalho numa loja com live no ar antes de decidir a hora de rodar.
func TestEnsaioNaoEscreveNada(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 10})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	dreno.carrinhos = []CartWithLegacyReservations{{CartID: "cart-1", StoreID: "loja-1", Rows: 2, Units: 5}}
	dreno.linhas["cart-1"] = []LegacyReservationRow{
		{ID: "r1", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 3},
		{ID: "r2", CartID: "cart-1", ExternalProductID: "ext-p1", Quantity: 2},
	}
	antes := legado.estoque("ext-p1")

	rel, err := svc.DrainLegacyReservations(ctx, "loja-1", true, 0)
	if err != nil {
		t.Fatalf("ensaio: %v", err)
	}
	if !rel.DryRun || rel.Carts != 1 || rel.Units != 5 {
		t.Errorf("o ensaio precisa dizer o tamanho do trabalho: %+v", rel)
	}
	if legado.criacoes != 0 || legado.entradas != 0 {
		t.Errorf("o ensaio escreveu no ERP: criações=%d entradas=%d", legado.criacoes, legado.entradas)
	}
	if legado.estoque("ext-p1") != antes {
		t.Error("o ensaio mexeu no estoque")
	}
	if len(dreno.revertidas) != 0 || len(dreno.claimadas) != 0 {
		t.Error("o ensaio mexeu no banco")
	}
}

// ─── Lotes ──────────────────────────────────────────────────────────────────

// 126 carrinhos são cerca de 850 chamadas contra um teto de 30 por minuto. O
// limite existe para drenar em lotes sem monopolizar a cota de uma loja que
// ainda está vendendo.
func TestLimiteCortaAPassada(t *testing.T) {
	svc, repo, legado, dreno := montarDrenagem(map[string]int{"ext-p1": 100})
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("cart-%d", i)
		repo.criarCarrinho(id, item("p1", 1))
		dreno.carrinhos = append(dreno.carrinhos, CartWithLegacyReservations{CartID: id, StoreID: "loja-1", Rows: 1, Units: 1})
		dreno.linhas[id] = []LegacyReservationRow{{ID: "r" + id, CartID: id, ExternalProductID: "ext-p1", Quantity: 1}}
	}

	rel, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 3)
	if err != nil {
		t.Fatalf("drenagem: %v", err)
	}
	if len(rel.Outcomes) != 3 {
		t.Errorf("processou %d carrinhos, quero 3", len(rel.Outcomes))
	}
	if legado.entradas != 3 {
		t.Errorf("entradas = %d, quero 3", legado.entradas)
	}
	// Mas o relatório diz o trabalho TOTAL, não só o do lote — senão não dá para
	// saber quanto falta.
	if rel.Carts != 10 || rel.Units != 10 {
		t.Errorf("o relatório precisa dizer o total pendente: carrinhos=%d unidades=%d", rel.Carts, rel.Units)
	}
}

// ─── O caso da cantodaart, no tamanho real ──────────────────────────────────

// 126 carrinhos, 462 linhas, 690 unidades — os números lidos do banco de
// produção em 26/08. O que se afirma aqui é a conservação: o disponível de cada
// produto termina exatamente onde começou.
func TestDrenagemNoTamanhoDaCantodaart(t *testing.T) {
	const carrinhos, linhasPorCarrinho = 126, 4
	saldos := map[string]int{}
	for p := 0; p < 30; p++ {
		saldos[fmt.Sprintf("ext-p%d", p)] = 200
	}
	svc, repo, legado, dreno := montarDrenagem(saldos)
	ctx := context.Background()

	disponivelAntes := map[string]int{}
	for ext := range saldos {
		disponivelAntes[ext] = legado.estoque(ext).disponivel()
	}

	unidades := 0
	for i := 0; i < carrinhos; i++ {
		cart := fmt.Sprintf("cart-%d", i)
		var itens []NonWaitlistedCartItem
		var linhas []LegacyReservationRow
		for j := 0; j < linhasPorCarrinho; j++ {
			prod := fmt.Sprintf("p%d", (i+j)%30)
			itens = append(itens, item(prod, 1))
			linhas = append(linhas, LegacyReservationRow{
				ID: fmt.Sprintf("r-%d-%d", i, j), CartID: cart,
				ExternalProductID: "ext-" + prod, Quantity: 1,
			})
			unidades++
		}
		repo.criarCarrinho(cart, itens...)
		dreno.carrinhos = append(dreno.carrinhos, CartWithLegacyReservations{
			CartID: cart, StoreID: "loja-1", Rows: linhasPorCarrinho, Units: linhasPorCarrinho,
		})
		dreno.linhas[cart] = linhas
	}

	rel, err := svc.DrainLegacyReservations(ctx, "loja-1", false, 0)
	if err != nil {
		t.Fatalf("drenagem: %v", err)
	}
	if rel.Failed != 0 {
		t.Errorf("%d carrinho(s) falharam", rel.Failed)
	}
	if rel.RowsReversed != carrinhos*linhasPorCarrinho {
		t.Errorf("linhas revertidas = %d, quero %d", rel.RowsReversed, carrinhos*linhasPorCarrinho)
	}
	if rel.OrdersCreated != carrinhos {
		t.Errorf("pedidos criados = %d, quero %d (um por carrinho)", rel.OrdersCreated, carrinhos)
	}
	// A conservação: nem uma unidade a mais nem a menos ficou disponível.
	for ext, antes := range disponivelAntes {
		if depois := legado.estoque(ext).disponivel(); depois != antes {
			t.Errorf("produto %s: disponivel %d → %d — a troca de guarda tem de ser "+
				"neutra", ext, antes, depois)
		}
	}
}
