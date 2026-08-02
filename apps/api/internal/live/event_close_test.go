package live

// RN-05 / CA-05.2 — o teto fecha o evento de QUALQUER tipo.
//
// Até aqui o sweep de ends_at só enxergava evento com sessão de post/reel/story:
// o comentário da query dizia, textualmente, que a restrição existia para "não
// auto-encerrar lives agendadas que o lojista quer manter rodando além do
// horário nominal". A RN-05 revoga essa decisão — ends_at virou o teto
// contratual da campanha. Com o filtro no lugar, um evento de live com ends_at
// vencido nunca fechava e seus carrinhos nunca ganhavam prazo, porque a RN-04
// mantém expires_at NULL enquanto o evento está aberto.

import (
	"context"
	"testing"
)

func seedEventPastEndsAt(t *testing.T, ctx context.Context, storeID, title string, sessionType *string) string {
	t.Helper()
	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active',$2, now() - interval '1 hour') RETURNING id::text`,
		storeID, title,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed evento vencido: %v", err)
	}
	if sessionType != nil {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO live_sessions (event_id, status, type, sequence_order)
			 VALUES ($1,'active',$2,1)`, eventID, *sessionType,
		); err != nil {
			t.Fatalf("seed sessao: %v", err)
		}
	}
	return eventID
}

func TestListEventsPastEndsAtCoversEveryType(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "close-alltypes")

	live := "live"
	post := "post"
	liveEvent := seedEventPastEndsAt(t, ctx, storeID, "Live vencida", &live)
	postEvent := seedEventPastEndsAt(t, ctx, storeID, "Post vencido", &post)
	// Evento sem sessão nenhuma é estado alcançável hoje (Create só cria sessão
	// quando vem plataforma + mídia). Ele também precisa fechar.
	orphanEvent := seedEventPastEndsAt(t, ctx, storeID, "Sem sessao", nil)

	// Um evento ainda dentro da janela não pode aparecer.
	var futureEvent string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','Ainda aberto', now() + interval '2 days') RETURNING id::text`,
		storeID,
	).Scan(&futureEvent); err != nil {
		t.Fatalf("seed evento futuro: %v", err)
	}

	refs, err := testRepo.ListEventsPastEndsAt(ctx, 500)
	if err != nil {
		t.Fatalf("ListEventsPastEndsAt: %v", err)
	}
	got := map[string]bool{}
	for _, r := range refs {
		got[r.EventID] = true
	}

	for _, tc := range []struct {
		id     string
		what   string
		expect bool
	}{
		{liveEvent, "evento de LIVE com ends_at vencido", true},
		{postEvent, "evento de post com ends_at vencido", true},
		{orphanEvent, "evento SEM sessão com ends_at vencido", true},
		{futureEvent, "evento ainda dentro da janela", false},
	} {
		if got[tc.id] != tc.expect {
			t.Errorf("%s: presente=%v, queria %v", tc.what, got[tc.id], tc.expect)
		}
	}
}
