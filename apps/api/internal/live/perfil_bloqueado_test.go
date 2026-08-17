package live

// O bloqueio de perfil é a promessa inteira da feature: o arroba bloqueado
// "jamais consegue processar algo dentro do LiveCart". O fake do repositório já
// tinha o campo `blockedHandles` desde o começo, mas NENHUM teste o usava — a
// aplicação do bloqueio nunca foi travada.
//
// O caso real: a lojista usa contas secundárias no Instagram para INSTRUIR a
// audiência ("manda 1042 2", ensinando o formato). O LiveCart lê isso como
// intenção de compra e abre pedido no nome dela. Bloquear a conta tem de matar
// o pedido, não só esconder da tela.
//
// Este funil é o único ponto de entrada de mensagem que cria carrinho:
// comentário de live, comentário de post e resposta de story (que chega por DM
// e é convertida em ProcessInstagramCommentInput) desembocam todos aqui. Travar
// aqui é travar "qualquer canal".

import (
	"context"
	"testing"
)

// setupCompra monta o cenário mínimo que GERA carrinho: produto com estoque,
// sessão viva, evento de live. É a linha de base contra a qual o bloqueio é
// medido — sem ela o teste passaria por qualquer motivo.
func setupCompra(t *testing.T) (*fakeIngestRepo, *fakeCommentCore, *Service) {
	t.Helper()
	repo := newFakeIngestRepo()
	repo.products["1042"] = &ProductRow{ID: "p1", Keyword: "1042", Price: 8990, Stock: 10, Name: "Vestido"}
	core := &fakeCommentCore{
		session:   &SessionOutput{ID: "sess1"},
		event:     liveEvent(),
		addResult: AddToCartOutput{CartID: "cart1", CartToken: "tok1", IsNewCart: true, TotalItems: 1, TotalCents: 8990},
	}
	s := newCommentTestService(repo, core)
	s.stockReserver = &fakeStockReserver{}
	return repo, core, s
}

func TestPerfilBloqueado_NaoGeraPedido(t *testing.T) {
	ctx := context.Background()
	repo, core, s := setupCompra(t)

	// Como o comment.go normaliza antes de consultar: minúsculo, sem @.
	repo.blockedHandles["cantodaart.oficial"] = true

	err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
		CommentID: "c1",
		MediaID:   "m1",
		UserID:    "u1",
		Username:  "cantodaart.oficial",
		Text:      "manda 1042 2",
	})
	if err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 0 {
		t.Errorf("perfil bloqueado gerou %d chamada(s) de AddToCart — é o pedido "+
			"indevido que motivou a feature", len(core.addCalls))
	}
	if len(repo.decremented) != 0 {
		t.Errorf("perfil bloqueado baixou estoque: %+v", repo.decremented)
	}
	if len(repo.createdWaitlist) != 0 {
		t.Errorf("perfil bloqueado entrou na fila de espera: %+v", repo.createdWaitlist)
	}

	// A mensagem CONTINUA sendo gravada, marcada como bloqueada. Descartá-la
	// deixaria o lojista sem saber que a conta segue tentando comprar — e é o
	// que a aba do evento mostra como "ignorado".
	if len(repo.createdComments) != 1 {
		t.Fatalf("esperava 1 comentário gravado, houve %d", len(repo.createdComments))
	}
	if got := repo.createdComments[0].Result; got != "blocked" {
		t.Errorf("comentário gravado com result=%q, esperava \"blocked\"", got)
	}
	if repo.createdComments[0].HasPurchaseIntent {
		t.Error("comentário de perfil bloqueado foi marcado com intenção de compra")
	}
}

// A outra metade, sem a qual "bloquear tudo" passaria: o MESMO comentário, de um
// arroba não bloqueado, tem de gerar pedido.
func TestPerfilNaoBloqueado_ContinuaComprando(t *testing.T) {
	ctx := context.Background()
	repo, core, s := setupCompra(t)

	repo.blockedHandles["cantodaart.oficial"] = true // outra conta bloqueada

	err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
		CommentID: "c2",
		MediaID:   "m1",
		UserID:    "u2",
		Username:  "compradora.real",
		Text:      "manda 1042 2",
	})
	if err != nil {
		t.Fatalf("ProcessInstagramComment: %v", err)
	}

	if len(core.addCalls) != 1 {
		t.Fatalf("compradora não bloqueada teve %d chamada(s) de AddToCart, "+
			"esperava 1 — o bloqueio pegou quem não devia", len(core.addCalls))
	}
	if core.addCalls[0].PlatformHandle != "compradora.real" {
		t.Errorf("carrinho saiu no nome de %q", core.addCalls[0].PlatformHandle)
	}
}

// O arroba chega do webhook do Instagram como o Meta manda, e o lojista digita
// no painel do jeito que lembra. As duas pontas têm de casar: se o bloqueio só
// funcionasse para a forma exata gravada, ele falharia em silêncio — a lojista
// veria o perfil na lista de bloqueados e o pedido apareceria de novo na live
// seguinte, que é o pior desfecho possível para esta feature.
func TestPerfilBloqueado_CasaIndependenteDeCaixaEArroba(t *testing.T) {
	ctx := context.Background()

	for _, comoVeioNoWebhook := range []string{
		"CantoDaArt.Oficial",     // Meta mandando com maiúsculas
		"@cantodaart.oficial",    // arroba colado no username
		"  cantodaart.oficial  ", // espaço em volta
	} {
		t.Run(comoVeioNoWebhook, func(t *testing.T) {
			repo, core, s := setupCompra(t)
			repo.blockedHandles["cantodaart.oficial"] = true

			err := s.ProcessInstagramComment(ctx, ProcessInstagramCommentInput{
				CommentID: "c-" + comoVeioNoWebhook,
				MediaID:   "m1",
				UserID:    "u1",
				Username:  comoVeioNoWebhook,
				Text:      "manda 1042 2",
			})
			if err != nil {
				t.Fatalf("ProcessInstagramComment: %v", err)
			}

			if len(core.addCalls) != 0 {
				t.Errorf("username %q furou o bloqueio e gerou pedido", comoVeioNoWebhook)
			}
		})
	}
}
