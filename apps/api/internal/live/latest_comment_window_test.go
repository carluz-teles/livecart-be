package live

// N9/RN-37 — GetLatestCommentIDByUser é a query que escolhe QUAL comentário
// recebe o link de checkout por private reply. Ela não tinha filtro de idade
// nenhum: numa campanha de uma semana, o disparo em massa do fechamento pegava
// o comentário do primeiro dia, e o private reply do Instagram vale 7 dias.

import (
	"context"
	"testing"
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
		`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,'active',$2,'multi') RETURNING id::text`,
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

		got, err := testRepo.GetLatestCommentIDByUser(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestCommentIDByUser: %v", err)
		}
		if got != "c-recente" {
			t.Errorf("escolheu %q, quero \"c-recente\"", got)
		}
	})

	t.Run("so comentario vencido devolve vazio", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-stale")
		seedComment(t, ctx, eventID, "c-8-dias", 8)

		got, err := testRepo.GetLatestCommentIDByUser(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestCommentIDByUser: %v", err)
		}
		if got != "" {
			t.Errorf("escolheu %q — o private reply para esse comentario nao sai mais", got)
		}
	})

	t.Run("na borda de 7 dias ainda vale", func(t *testing.T) {
		eventID := seedCommentsFixture(t, ctx, "cmt-window-edge")
		seedComment(t, ctx, eventID, "c-6-dias", 6)

		got, err := testRepo.GetLatestCommentIDByUser(ctx, eventID, "ig-buyer")
		if err != nil {
			t.Fatalf("GetLatestCommentIDByUser: %v", err)
		}
		if got != "c-6-dias" {
			t.Errorf("escolheu %q, quero \"c-6-dias\"", got)
		}
	})
}
