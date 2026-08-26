package erp

// O caminho de volta: o lojista mexe no pedido pelo painel, o carrinho segue.
//
// O que se afirma aqui é a direção da autoridade. O pedido é o documento de
// venda; o carrinho é a projeção dele para a compradora. Quando os dois
// divergem, o pedido vence — e o carrinho tem de refletir, porque é ele que ela
// vê e paga.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

// cartSyncSimulado é o lado de fora do reflexo: catálogo da loja e itens do
// carrinho.
type cartSyncSimulado struct {
	repo *repoSimulado
	// catalogo mapeia id no ERP → id local. O que não está aqui é produto que a
	// loja nunca importou.
	catalogo     map[string]string
	importados   []string
	falharImport error
	// naoResolve força o erro de leitura do catálogo.
	naoResolve error
}

func (c *cartSyncSimulado) ResolveLocalProduct(_ context.Context, _, ext string) (string, bool, error) {
	if c.naoResolve != nil {
		return "", false, c.naoResolve
	}
	id, ok := c.catalogo[ext]
	return id, ok, nil
}

func (c *cartSyncSimulado) ImportProductFromERP(_ context.Context, _, ext string) (string, error) {
	if c.falharImport != nil {
		return "", c.falharImport
	}
	id := "prod-importado-" + ext
	c.catalogo[ext] = id
	c.importados = append(c.importados, ext)
	return id, nil
}

func (c *cartSyncSimulado) SetCartItemQuantity(_ context.Context, cartID, productID string, qtd int, preco int64) error {
	c.repo.mu.Lock()
	defer c.repo.mu.Unlock()
	cart, ok := c.repo.carrinhos[cartID]
	if !ok {
		return fmt.Errorf("carrinho %s não existe", cartID)
	}
	for i := range cart.itens {
		if cart.itens[i].ProductID == productID {
			cart.itens[i].Quantity = qtd
			cart.itens[i].UnitPrice = preco
			return nil
		}
	}
	ext := ""
	for e, id := range c.catalogo {
		if id == productID {
			ext = e
		}
	}
	cart.itens = append(cart.itens, NonWaitlistedCartItem{
		ID: "ci-" + productID, CartID: cartID, ProductID: productID,
		Quantity: qtd, UnitPrice: preco, ProductExternalID: ext,
		ProductName: "Produto " + productID,
	})
	return nil
}

func (c *cartSyncSimulado) RemoveCartItem(_ context.Context, cartID, productID string) error {
	c.repo.mu.Lock()
	defer c.repo.mu.Unlock()
	cart, ok := c.repo.carrinhos[cartID]
	if !ok {
		return nil
	}
	restantes := cart.itens[:0]
	for _, it := range cart.itens {
		if it.ProductID != productID {
			restantes = append(restantes, it)
		}
	}
	cart.itens = restantes
	return nil
}

func montarReflexo(saldos map[string]int) (*Service, *repoSimulado, *erpSimulado, *cartSyncSimulado) {
	s, r, e, _ := montar(saldos)
	cs := &cartSyncSimulado{repo: r, catalogo: map[string]string{}}
	s.SetCartSyncCollaborators(cs)
	return s, r, e, cs
}

func itensDoCarrinho(r *repoSimulado, cartID string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	if c, ok := r.carrinhos[cartID]; ok {
		for _, it := range c.itens {
			out[it.ProductExternalID] = it.Quantity
		}
	}
	return out
}

// ─── O lojista acrescenta um produto ────────────────────────────────────────

// Produto que a loja JÁ tem importado: entra no carrinho direto.
func TestProdutoQueOLojistaAdicionouEntraNoCarrinho(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-p2": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	cs.catalogo["ext-p1"] = "p1"
	cs.catalogo["ext-p2"] = "p2"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	// O lojista soma 3 unidades de outro produto pelo painel.
	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-p2", 3, "cliente pediu por DM")

	rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if got := itensDoCarrinho(repo, "cart-1")["ext-p2"]; got != 3 {
		t.Errorf("o carrinho ficou com %d un. do produto que o lojista acrescentou, "+
			"quero 3 — sem isso a compradora paga por algo que não vê", got)
	}
	if len(rel.Changes) != 1 || rel.Changes[0].Kind != "added" {
		t.Errorf("mudanças = %+v, quero uma de 'added'", rel.Changes)
	}
	if rel.Imported != 0 {
		t.Errorf("importou %d produtos, quero 0 — este já estava cadastrado", rel.Imported)
	}
}

// Produto que a loja NUNCA importou: é cadastrado agora, como se o lojista o
// tivesse trazido pela tela.
func TestProdutoSemCadastroEhImportadoAutomaticamente(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-novo": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	cs.catalogo["ext-p1"] = "p1"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-novo", 2, "produto novo")

	rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if rel.Imported != 1 {
		t.Fatalf("importados = %d, quero 1", rel.Imported)
	}
	if len(cs.importados) != 1 || cs.importados[0] != "ext-novo" {
		t.Errorf("importou %v, quero [ext-novo]", cs.importados)
	}
	if got := itensDoCarrinho(repo, "cart-1")["ext-novo"]; got != 2 {
		t.Errorf("o produto importado entrou com %d un., quero 2", got)
	}
}

// Um produto que não dá para importar não derruba o reflexo inteiro: as outras
// linhas ainda valem.
func TestImportacaoQueFalhaNaoDerrubaOResto(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-p2": 20, "ext-ruim": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	cs.catalogo["ext-p1"] = "p1"
	cs.catalogo["ext-p2"] = "p2"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	orderID := repo.carrinho("cart-1").externalOrderID
	erp.adicionarLinhaDoLojista(orderID, "ext-p2", 4, "ok")
	erp.adicionarLinhaDoLojista(orderID, "ext-ruim", 1, "produto problemático")
	cs.falharImport = errors.New("produto sem preço no ERP")

	rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if got := itensDoCarrinho(repo, "cart-1")["ext-p2"]; got != 4 {
		t.Errorf("a linha boa não entrou (%d un.) por causa da ruim", got)
	}
	if rel.Imported != 0 {
		t.Errorf("contou %d importados apesar da falha", rel.Imported)
	}
}

// ─── O lojista altera a quantidade ──────────────────────────────────────────

func TestQuantidadeAlteradaNoPainelRefleteNoCarrinho(t *testing.T) {
	for _, caso := range []struct{ de, para int }{{2, 5}, {5, 1}, {3, 3}} {
		t.Run(fmt.Sprintf("%d_para_%d", caso.de, caso.para), func(t *testing.T) {
			svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 50})
			ctx := context.Background()
			repo.criarCarrinho("cart-1", item("p1", caso.de))
			cs.catalogo["ext-p1"] = "p1"
			_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", caso.de, 2000, "@maria")

			// O lojista corrige a quantidade no painel.
			orderID := repo.carrinho("cart-1").externalOrderID
			_ = erp.UpdateOrderItems(ctx, orderID, []providers.ERPOrderItem{
				{ProductID: "ext-p1", Quantity: caso.para, UnitPrice: 2000, Note: providers.LiveCartItemMarker},
			})

			if _, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1"); err != nil {
				t.Fatalf("reflexo: %v", err)
			}
			if got := itensDoCarrinho(repo, "cart-1")["ext-p1"]; got != caso.para {
				t.Errorf("carrinho ficou com %d, quero %d — o pedido manda na grade", got, caso.para)
			}
		})
	}
}

// ─── O lojista remove uma linha ─────────────────────────────────────────────

func TestLinhaRemovidaNoPainelSaiDoCarrinho(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-p2": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2), item("p2", 3))
	cs.catalogo["ext-p1"] = "p1"
	cs.catalogo["ext-p2"] = "p2"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")

	// O lojista apaga a linha do p2.
	orderID := repo.carrinho("cart-1").externalOrderID
	_ = erp.UpdateOrderItems(ctx, orderID, []providers.ERPOrderItem{
		{ProductID: "ext-p1", Quantity: 2, UnitPrice: 2000, Note: providers.LiveCartItemMarker},
	})

	if _, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	itens := itensDoCarrinho(repo, "cart-1")
	if _, existe := itens["ext-p2"]; existe {
		t.Errorf("o item apagado no painel continuou no carrinho: %v — a compradora "+
			"seguiria pagando por ele", itens)
	}
	if itens["ext-p1"] != 2 {
		t.Errorf("a outra linha foi afetada: %v", itens)
	}
}

// ─── O que o reflexo NÃO faz ────────────────────────────────────────────────

// Não mexe no contador local de estoque. Ele é espelho do `disponivel`, e o
// `disponivel` já contou a mudança do lojista no instante em que ela entrou no
// pedido. Descontar aqui também contaria duas vezes.
func TestReflexoNaoMexeNoContadorLocal(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-p2": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	repo.estoque["p2"] = 10
	cs.catalogo["ext-p1"] = "p1"
	cs.catalogo["ext-p2"] = "p2"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")

	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-p2", 4, "do lojista")
	if _, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if repo.estoque["p2"] != 10 {
		t.Errorf("contador local de p2 = %d, quero 10 intacto — o espelho do "+
			"disponível já desconta essas unidades, e descontar aqui contaria duas "+
			"vezes", repo.estoque["p2"])
	}
}

// Carrinho pago não é tocado: a grade daquela venda está fechada, e mexer nela
// mudaria o que a compradora pagou.
func TestReflexoNaoTocaCarrinhoPago(t *testing.T) {
	svc, repo, erp, cs := montarReflexo(map[string]int{"ext-p1": 20, "ext-p2": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	cs.catalogo["ext-p1"] = "p1"
	cs.catalogo["ext-p2"] = "p2"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 2, 2000, "@maria")
	_ = svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil)

	erp.adicionarLinhaDoLojista(repo.carrinho("cart-1").externalOrderID, "ext-p2", 5, "depois do pagamento")
	rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if rel.Skipped == "" {
		t.Errorf("o reflexo agiu sobre um carrinho pago: %+v", rel.Changes)
	}
	if _, existe := itensDoCarrinho(repo, "cart-1")["ext-p2"]; existe {
		t.Error("mudou a grade de uma venda já paga")
	}
}

// Escrita nossa em voo: o reflexo desiste. Ela vai reenviar a grade do banco, e
// o próximo reflexo lê o resultado — insistir aqui faria os dois se desfazerem.
func TestReflexoDesisteComEscritaEmVoo(t *testing.T) {
	svc, repo, _, cs := montarReflexo(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	cs.catalogo["ext-p1"] = "p1"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "@maria")
	_, _ = repo.TransitionCartERPOrderState(ctx, "cart-1", OrderStateOpen, OrderStateMutating)

	rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatalf("reflexo: %v", err)
	}
	if rel.Skipped == "" {
		t.Error("o reflexo agiu por cima de uma escrita em voo")
	}
}

// Reflexo sobre um pedido que já bate com o carrinho não muda nada.
func TestReflexoSemDiferencaNaoMudaNada(t *testing.T) {
	svc, repo, _, cs := montarReflexo(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 3))
	cs.catalogo["ext-p1"] = "p1"
	_ = svc.ReserveStockInERP(ctx, "loja-1", "cart-1", "ev-1", "p1", 3, 2000, "@maria")

	for i := 0; i < 3; i++ {
		rel, err := svc.SyncCartFromERPOrder(ctx, "cart-1", "loja-1")
		if err != nil {
			t.Fatalf("reflexo %d: %v", i, err)
		}
		if len(rel.Changes) != 0 {
			t.Errorf("reflexo %d mexeu no carrinho sem diferença: %+v", i, rel.Changes)
		}
	}
}
