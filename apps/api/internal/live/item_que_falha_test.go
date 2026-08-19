package live

// Um item que quebra não pode levar o comentário inteiro embora.
//
// A leitura por item trouxe um estado que não existia antes: sucesso PARCIAL.
// Antes, um comentário era um produto — se o AddToCart falhava, nada tinha
// entrado e devolver erro estava certo, porque o retry do asynq recomeçaria do
// zero.
//
// Com vários produtos por comentário, devolver erro no meio do laço deixa o que
// já entrou dentro do carrinho e sai ANTES de mandar a DM. E o retry não
// conserta: o comentário já está gravado, então ele bate no portão de dedup e
// sai calado. Resultado: item no carrinho, compradora sem link, ninguém avisado.
//
// É a mesma família do defeito que a live de 17/08 mostrou (as duas DMs perdidas
// no timeout do Tiny), e o mais caro deste sistema: quando a compradora não
// recebe a mensagem, ela não tem como pagar.

import (
	"context"
	"testing"
)

func TestItemQueFalhaNaoApagaOsOutros(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:      &SessionOutput{ID: "sess1"},
		event:        liveEvent(),
		addResult:    AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true, TotalItems: 5},
		addErrOnCall: 2, // o SEGUNDO item quebra
	}
	s := newCommentTestService(repo, core)
	stock := &fakeStockReserver{}
	s.stockReserver = stock

	err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 x5 1005 x3",
	})
	if err != nil {
		t.Fatalf("o comentário inteiro falhou por causa de um item: %v — o primeiro "+
			"produto já estava no carrinho e a compradora não recebeu nada", err)
	}

	if len(core.addCalls) != 2 {
		t.Fatalf("os dois itens precisam ser tentados: %+v", core.addCalls)
	}

	// O estoque do item que falhou volta; o do que entrou, não.
	var devolvido bool
	for _, m := range repo.incremented {
		if m.productID == "p1005" && m.quantity == 3 {
			devolvido = true
		}
		if m.productID == "p1000" {
			t.Errorf("devolveu estoque do item que ENTROU (%+v)", m)
		}
	}
	if !devolvido {
		t.Errorf("o estoque do item que falhou não voltou: %+v", repo.incremented)
	}
}

// Todos os itens falhando: aí não há nada a anunciar, e o erro sobe para o
// retry fazer sentido.
func TestTodosOsItensFalhandoDevolveErro(t *testing.T) {
	repo := lojaComDoisProdutos()
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1"},
		addErr:    context.DeadlineExceeded,
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}

	if err := s.ProcessInstagramComment(context.Background(), ProcessInstagramCommentInput{
		CommentID: "c1", MediaID: "m1", UserID: "u1", Username: "ana",
		Text: "1000 x5 1005 x3",
	}); err == nil {
		t.Error("nenhum item entrou e o erro não subiu — sem ele o asynq não tenta de novo")
	}
}
