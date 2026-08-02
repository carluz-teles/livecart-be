package live

// N9/RN-37 + RN-38 — GetLatestReplyTarget é a query que escolhe QUAL comentário
// recebe o link de checkout por private reply. Ela não tinha filtro de idade
// nenhum: numa campanha de uma semana, o disparo em massa do fechamento pegava
// o comentário do primeiro dia, e o private reply do Instagram vale 7 dias.
//
// O filtro de 7 dias MORAVA nesta query e saiu (RN-38). Com ele, um comprador
// cujo único comentário venceu era indistinguível de um comprador que nunca
// comentou: os dois davam "". Agora a query devolve o comentário e a IDADE
// dele, e quem decide — e REGISTRA o motivo da não entrega — é o domínio da
// notificação. O que este arquivo passa a guardar é justamente isso: a idade
// tem de vir junto, senão os dois motivos voltam a colapsar num só.

import (
	"context"
	"testing"
	"time"
)

func seedCommentsFixture(t *testing.T, ctx context.Context, slug string) (eventID string) {
	t.Helper()
	var storeID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1,$1) RETURNING id::text`, slug,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1,'active',$2, now() + interval '7 days') RETURNING id::text`,
		storeID, slug,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID
}

func seedComment(t *testing.T, ctx context.Context, eventID, commentID string, daysAgo float64) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO live_comments (event_id, platform, platform_comment_id, platform_user_id, platform_handle, text, created_at)
		 VALUES ($1,'instagram',$2,'ig-buyer','@buyer','eu quero', now() - make_interval(days => $3::int))`,
		eventID, commentID, int(daysAgo),
	); err != nil {
		t.Fatalf("seed comment %s: %v", commentID, err)
	}
}

func TestGetLatestCommentIDRespeitaAJanelaDe7Dias(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	t.Run("prefere o mais novo dentro da janela", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-ok")
		seedComment(t, ctx, eventID, "c-antigo", 9)
		seedComment(t, ctx, eventID, "c-recente", 2)

		got, err := testRepo.GetLatestReplyTarget(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestReplyTarget: %v", err)
		}
		if got.CommentID != "c-recente" {
			t.Errorf("escolheu %q, quero \"c-recente\"", got.CommentID)
		}
		if got.CreatedAt == nil {
			t.Error("veio sem a idade do comentário — sem ela não dá para classificar a não entrega")
		}
	})

	t.Run("comentario vencido volta COM a idade, para virar motivo", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-stale")
		seedComment(t, ctx, eventID, "c-8-dias", 8)

		got, err := testRepo.GetLatestReplyTarget(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestReplyTarget: %v", err)
		}
		// Devolver "" aqui era o bug: virava "este cliente não tem comentário",
		// que é outro motivo e outra instrução para o lojista.
		if got.CommentID != "c-8-dias" {
			t.Errorf("escolheu %q, quero o comentario vencido para poder classifica-lo", got.CommentID)
		}
		if got.CreatedAt == nil || time.Since(*got.CreatedAt) <= 7*24*time.Hour {
			t.Errorf("idade do comentario nao permite concluir que venceu: %v", got.CreatedAt)
		}
	})

	t.Run("sem comentario nenhum devolve alvo vazio", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-none")

		got, err := testRepo.GetLatestReplyTarget(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestReplyTarget: %v", err)
		}
		if got.CommentID != "" || got.CreatedAt != nil {
			t.Errorf("alvo = %+v, quero vazio", got)
		}
	})

	t.Run("na borda de 7 dias ainda vale", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-edge")
		seedComment(t, ctx, eventID, "c-6-dias", 6)

		got, err := testRepo.GetLatestReplyTarget(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestReplyTarget: %v", err)
		}
		if got.CommentID != "c-6-dias" {
			t.Errorf("escolheu %q, quero \"c-6-dias\"", got.CommentID)
		}
	})
}
