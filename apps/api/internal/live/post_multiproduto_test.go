package live

// Post e story ficaram com UM item por comentário — e a quantidade desse item
// estava errada.
//
// A leitura por item entregou `[]PurchaseItem`; o ramo de post-commerce colapsa
// isso num item só, porque suas regras (whitelist da sessão, produto único,
// resposta de indisponível) decidem pelo comentário inteiro. O colapso usava
// `intent.Quantity`, que é a SOMA de todos os itens.
//
// Consequência: "1000 5x 1005 3x" num post virava pedido de OITO unidades do
// 1000. O código anterior dava cinco — também errado, mas sem inflar. Trocar um
// erro por um erro maior, em silêncio, dentro do carrinho de alguém.

import (
	"context"
	"testing"
)

func postComDoisProdutos(t *testing.T) (*fakeIngestRepo, *fakeCommentCore, *Service) {
	t.Helper()
	repo := newFakeIngestRepo()
	repo.products["1000"] = &ProductRow{ID: "p1000", Keyword: "1000", Price: 1000, Stock: 50, Name: "Vaso"}
	repo.products["1005"] = &ProductRow{ID: "p1005", Keyword: "1005", Price: 2000, Stock: 50, Name: "Prato"}
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1", Type: "post"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true},
	}
	core.scriptWhitelist(
		SessionProductOutput{ProductID: "p1000", ProductActive: true, Stock: 50, Keyword: "1000", Name: "Vaso"},
		SessionProductOutput{ProductID: "p1005", ProductActive: true, Stock: 50, Keyword: "1005", Name: "Prato"},
	)
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}
	return repo, core, s
}

func TestPostNaoSomaAsQuantidadesDosItens(t *testing.T) {
	_, core, s := postComDoisProdutos(t)

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 5x 1005 3x",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 1 {
		t.Fatalf("post-commerce precisa seguir com UM item: %+v", core.addCalls)
	}
	got := core.addCalls[0]
	if got.Quantity != 5 {
		t.Errorf("quantidade = %d; esperava 5 — a do item que resolveu neste produto. "+
			"Somar os itens (5+3) infla o pedido dela para 8 unidades", got.Quantity)
	}
	if got.ProductID != "p1000" {
		t.Errorf("produto = %s; esperava p1000, o primeiro citado", got.ProductID)
	}
}

// Um item só continua exato — a correção não pode ter mexido no caso comum.
func TestPostComUmItemContinuaExato(t *testing.T) {
	_, core, s := postComDoisProdutos(t)

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1005 x4",
	}); err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}
	if len(core.addCalls) != 1 || core.addCalls[0].ProductID != "p1005" || core.addCalls[0].Quantity != 4 {
		t.Fatalf("pedido = %+v; esperava p1005 x4", core.addCalls)
	}
}
