package live

// Um comentário, vários produtos — e o fim do produto em destaque como resposta
// para código que não existe.
//
// O parser já lê "1000 5x 1005 3x" como dois itens (purchase_parse_test.go).
// Estes testes cobrem o outro lado: que a ingestão FAZ alguma coisa com o
// segundo item. Antes ela não fazia — resolvia um produto, adicionava um item e
// devolvia. O segundo produto e a quantidade dele sumiam sem log, e a
// compradora só descobria no checkout.

import (
	"context"
	"testing"
)

func lojaComDoisProdutos() *fakeIngestRepo {
	repo := newFakeIngestRepo()
	repo.products["1000"] = &ProductRow{ID: "p1000", Keyword: "1000", Price: 1000, Stock: 50, Name: "Vaso"}
	repo.products["1005"] = &ProductRow{ID: "p1005", Keyword: "1005", Price: 2000, Stock: 50, Name: "Prato"}
	return repo
}

// O caso que a lojista relatou, inteiro.
func TestComentarioComDoisProdutosEntraOsDois(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true, TotalItems: 8, TotalCents: 11000},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 5x 1005 3x",
	})
	if err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 2 {
		t.Fatalf("AddToCart chamado %d vez(es) para um comentário com dois produtos: %+v — "+
			"o segundo produto some sem log e a compradora só descobre no checkout",
			len(core.addCalls), core.addCalls)
	}

	primeiro, segundo := core.addCalls[0], core.addCalls[1]
	if primeiro.ProductID != "p1000" || primeiro.Quantity != 5 {
		t.Errorf("primeiro item = %s x%d; esperava p1000 x5", primeiro.ProductID, primeiro.Quantity)
	}
	if segundo.ProductID != "p1005" || segundo.Quantity != 3 {
		t.Errorf("segundo item = %s x%d; esperava p1005 x3 — a quantidade é DELE, "+
			"não a do primeiro código", segundo.ProductID, segundo.Quantity)
	}
}

// A ordem é a do texto, porque é a ordem em que a compradora pensou.
func TestComentarioComDoisProdutosPreservaAOrdemDoTexto(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1005 x2 1000 x1",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 2 || core.addCalls[0].ProductID != "p1005" {
		t.Fatalf("ordem dos itens = %+v; o texto citou 1005 primeiro", core.addCalls)
	}
}

// Estoque é reservado por item, com o número de cada um.
func TestCadaItemReservaOSeuEstoque(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 5x 1005 3x",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(repo.decremented) != 2 {
		t.Fatalf("baixas de estoque = %+v; esperava uma por item", repo.decremented)
	}
	if repo.decremented[0] != (stockMove{"p1000", 5}) || repo.decremented[1] != (stockMove{"p1005", 3}) {
		t.Errorf("baixas = %+v; esperava p1000 x5 e p1005 x3", repo.decremented)
	}
	if len(stock.erpCalls) != 2 {
		t.Errorf("reservas no ERP = %+v; o ERP precisa ver os dois itens", stock.erpCalls)
	}
}

// O carrinho é UM, então o contador de pedidos do evento sobe UMA vez.
func TestComentarioComDoisProdutosContaUmPedidoSo(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session: &SessionOutput{ID: "sess1"},
		event:   liveEvent(),
		// Segundo item cai no mesmo carrinho: IsNewCart continua true no fake,
		// então o contador só não duplica se ProcessComment contar por carrinho.
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 5x 1005 3x",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if repo.eventOrderIncs != 1 {
		t.Errorf("contador de pedidos do evento subiu %d vezes; um comentário é um "+
			"carrinho, e dois produtos não são dois pedidos", repo.eventOrderIncs)
	}
}

// Um item que não existe não pode derrubar o que existe.
func TestCodigoInexistenteNoMeioNaoDerrubaOsOutros(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 x2 9911 x1 1005 x3",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 2 {
		t.Fatalf("itens adicionados = %+v; o código inexistente do meio precisa ser "+
			"pulado, não encerrar o comentário", core.addCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// O produto em destaque deixou de responder por código citado.
//
// O fallback existe para o "EU QUERO" pelado, em que a compradora não nomeia
// nada e quem escolhe é a transmissão. Quando ela DIGITA um código, ela disse o
// que quer; se aquele código não existe nesta loja, entregar o produto em
// destaque é entregar outra coisa — e esse erro só aparece na hora de separar a
// caixa. Era um dos caminhos dos pedidos que ninguém fez na live de 16/08.
// ─────────────────────────────────────────────────────────────────────────────

func sessaoComDestaque(repo *fakeIngestRepo) *SessionOutput {
	destaque := "p-destaque"
	repo.productsByID[destaque] = &ProductRow{
		ID: destaque, Keyword: "1500", Price: 3000, Stock: 10, Name: "Produto em destaque",
	}
	return &SessionOutput{ID: "sess1", CurrentActiveProductID: &destaque}
}

func TestCodigoInexistenteNaoViraProdutoEmDestaque(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   sessaoComDestaque(repo),
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	// 9911 não existe nesta loja.
	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "9911 x2",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 0 {
		t.Errorf("comentário com código inexistente criou pedido de %+v — a compradora "+
			"digitou 9911 e receberia o produto em destaque, que é outra coisa",
			core.addCalls)
	}
}

// A outra metade da regra: o pedido SEM código continua caindo no destaque, que
// é para o que o fallback foi feito.
func TestPedidoSemCodigoContinuaCaindoNoDestaque(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   sessaoComDestaque(repo),
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "quero 2",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 1 || core.addCalls[0].ProductID != "p-destaque" {
		t.Fatalf("pedido sem código = %+v; esperava o produto em destaque x2 — é "+
			"exatamente para isso que o fallback existe", core.addCalls)
	}
	if core.addCalls[0].Quantity != 2 {
		t.Errorf("quantidade = %d; esperava 2", core.addCalls[0].Quantity)
	}
}

// E o falso positivo que motivou tudo: com destaque ativo, uma pergunta de preço
// não pode virar pedido.
func TestPerguntaDePrecoNaoViraPedidoNemComDestaqueAtivo(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   sessaoComDestaque(repo),
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	for _, texto := range []string{
		"valor 1000",
		"Qual o preço do 1000?",
		"quanto custa",
		"Esse cogumelo tem maior?",
		"Valor dos cogumelos 🍄 pequenos?",
	} {
		core.addCalls = nil
		if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
			CommentID: "c-" + texto, MediaID: "m1", UserID: "u1", Username: "ana", Text: texto,
		}); err != nil {
			t.Fatalf("ProcessInstagramComment(%q): %v", texto, err)
		}
		if len(core.addCalls) != 0 {
			t.Errorf("%q criou pedido %+v — some estoque e entra pedido no ERP por "+
				"uma pergunta", texto, core.addCalls)
		}
	}
}
