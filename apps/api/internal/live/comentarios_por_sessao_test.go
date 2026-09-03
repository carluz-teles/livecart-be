package live

// A SEGUNDA TRANSMISSÃO NÃO CABIA NA JANELA.
//
// A lista de comentários da campanha lê as N primeiras falas em ordem de
// chegada. Numa campanha guarda-chuva isso significa "as N primeiras da
// PRIMEIRA transmissão" — a live de segunda tem centenas de falas, e o post de
// quinta começa lá pela milésima.
//
// O sintoma não era uma lista curta: era um seletor que sumia. A tela montava
// as opções de transmissão a partir das falas que tinha em mão, todas da
// primeira, concluía "só existe uma transmissão" e escondia o filtro. O lojista
// não conseguia pedir a segunda porque a tela nem sabia que ela existia.
//
// Filtrar depois de ler não resolve: as falas da segunda transmissão nunca
// chegaram. O corte tem que ser do banco.

import (
	"context"
	"fmt"
	"testing"
)

func seedComentarioNaSessao(t *testing.T, ctx context.Context, eventID, sessionID, handle string, ordem int) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO live_comments
		   (event_id, session_id, platform, platform_comment_id, platform_user_id,
		    platform_handle, text, created_at)
		 VALUES ($1, $2::uuid, 'instagram', $3, 'ig-'||$4, $4, 'quero',
		         now() - make_interval(mins => $5::int))`,
		eventID, sessionID, fmt.Sprintf("c-%s-%d", handle, ordem), handle, 1000-ordem,
	); err != nil {
		t.Fatalf("seed comentário %s#%d: %v", handle, ordem, err)
	}
}

func TestListCommentsByEventFiltraPorSessao(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	live := seedSession(t, eventID, 1)
	post := seedSession(t, eventID, 2)

	// A live enche a janela sozinha; o post vem depois, como na produção.
	const naLive = 25
	for i := 0; i < naLive; i++ {
		seedComentarioNaSessao(t, ctx, eventID, live, "dany", i)
	}
	seedComentarioNaSessao(t, ctx, eventID, post, "tati", naLive)
	seedComentarioNaSessao(t, ctx, eventID, post, "jaque", naLive+1)

	t.Run("sem filtro a janela cabe só na primeira transmissão", func(t *testing.T) {
		// Exatamente o que a tela recebia: uma janela menor que a primeira
		// transmissão. Nenhuma fala do post aparece — e é por isso que o
		// seletor precisa das sessões do evento, não das falas carregadas.
		got, err := testRepo.ListCommentsByEvent(ctx, eventID, "", 10, 0)
		if err != nil {
			t.Fatalf("ListCommentsByEvent: %v", err)
		}
		if len(got) != 10 {
			t.Fatalf("janela de 10: got %d", len(got))
		}
		for _, c := range got {
			if c.SessionID != live {
				t.Fatalf("a janela vazou para outra transmissão: %s", c.SessionID)
			}
		}
	})

	t.Run("com filtro a segunda transmissão aparece inteira", func(t *testing.T) {
		got, err := testRepo.ListCommentsByEvent(ctx, eventID, post, 10, 0)
		if err != nil {
			t.Fatalf("ListCommentsByEvent: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("o post tem 2 falas: got %d", len(got))
		}
		for _, c := range got {
			if c.SessionID != post {
				t.Fatalf("veio fala de outra transmissão: %s", c.SessionID)
			}
		}
	})

	t.Run("filtro vazio continua sendo a campanha inteira", func(t *testing.T) {
		got, err := testRepo.ListCommentsByEvent(ctx, eventID, "", 200, 0)
		if err != nil {
			t.Fatalf("ListCommentsByEvent: %v", err)
		}
		if len(got) != naLive+2 {
			t.Fatalf("campanha inteira: esperado %d, got %d", naLive+2, len(got))
		}
	})

	t.Run("transmissão de outro evento não devolve nada", func(t *testing.T) {
		outro := seedEvent(t)
		alheia := seedSession(t, outro, 1)
		seedComentarioNaSessao(t, ctx, outro, alheia, "intrusa", 1)

		// O WHERE casa event_id E session_id: pedir a sessão do vizinho pelo
		// evento errado devolve vazio, não a fala dele.
		got, err := testRepo.ListCommentsByEvent(ctx, eventID, alheia, 10, 0)
		if err != nil {
			t.Fatalf("ListCommentsByEvent: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("vazou fala de outro evento: got %d", len(got))
		}
	})
}
