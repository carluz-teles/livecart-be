package live

// D18/D19/D20 + N9 — a resolução mídia → sessão → evento deixou de filtrar por
// status, e o polling deixou de ser cego para campanha agendada.
//
// O que estes testes travam:
//  1. campanha AGENDADA, transmissão ENCERRADA e campanha ENCERRADA resolvem —
//     sem resolver não há loja, e sem loja não há como responder ao comprador
//     nem registrar o comentário (o descarte silencioso do ANALISE_LOGS);
//  2. o polling — único caminho de captura para post sem webhook — enxerga a
//     campanha agendada, que era exatamente o caso em que "nunca fica em
//     silêncio" viraria silêncio total;
//  3. a carência de resposta tardia é 7 dias de verdade. A anterior prometia 2
//     e entregava 0, porque o mesmo WHERE exigia status='active' e o sweep de
//     ends_at flipa o evento para 'ended'.

import (
	"context"
	"fmt"
	"testing"
)

type mediaFixture struct {
	storeID   string
	eventID   string
	sessionID string
	mediaID   string
}

// seedMedia cria loja + evento + sessão + mídia com os status pedidos.
// endedAgo != 0 encerra o evento com ends_at a essa distância no passado.
func seedMedia(t *testing.T, ctx context.Context, slug, eventStatus, sessionStatus, sessionType string, endedDaysAgo float64) mediaFixture {
	t.Helper()
	f := mediaFixture{mediaID: "media_" + slug}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1,$1) RETURNING id::text`, slug,
	).Scan(&f.storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	// ends_at e NOT NULL desde a 000122: um evento sem teto deixou de existir.
	// O caso "ainda aberto" vira uma janela no futuro, que e o que ele sempre
	// significou de verdade.
	endsAt := "now() + interval '7 days'"
	if endedDaysAgo != 0 {
		endsAt = fmt.Sprintf("now() - interval '%f days'", endedDaysAgo)
	}
	if err := testPool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,$2,$3,%s) RETURNING id::text`, endsAt),
		f.storeID, eventStatus, slug,
	).Scan(&f.eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, type, sequence_order)
		 VALUES ($1,$2,$3,1) RETURNING id::text`,
		f.eventID, sessionStatus, sessionType,
	).Scan(&f.sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
		 VALUES ($1,'instagram',$2)`,
		f.sessionID, f.mediaID,
	); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	return f
}

// Os três casos da D18/D19/D20 chegam ao domínio em vez de sumirem na query.
func TestResolucaoPorMidiaIgnoraStatus(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name          string
		slug          string
		eventStatus   string
		sessionStatus string
	}{
		{"campanha agendada", "res-scheduled", "scheduled", SessionStatusActive},
		{"transmissao encerrada, campanha aberta", "res-session-ended", "active", SessionStatusEnded},
		{"campanha encerrada", "res-event-ended", "ended", SessionStatusEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedMedia(t, ctx, tc.slug, tc.eventStatus, tc.sessionStatus, SessionTypePost, 0)

			session, err := testRepo.GetSessionByPlatformLiveID(ctx, f.mediaID)
			if err != nil {
				t.Fatalf("GetSessionByPlatformLiveID: %v", err)
			}
			if session == nil {
				t.Fatal("sessao nao resolveu — o comentario seria descartado em silencio")
			}
			if session.Status != tc.sessionStatus {
				t.Errorf("session.Status = %q, quero %q (o dominio precisa do valor cru)", session.Status, tc.sessionStatus)
			}

			event, err := testRepo.GetEventByPlatformLiveID(ctx, f.mediaID)
			if err != nil {
				t.Fatalf("GetEventByPlatformLiveID: %v", err)
			}
			if event == nil {
				t.Fatal("evento nao resolveu — sem store_id nao ha como responder ao comprador")
			}
			if event.Status != tc.eventStatus {
				t.Errorf("event.Status = %q, quero %q", event.Status, tc.eventStatus)
			}
		})
	}
}

// O polling é o único caminho de captura para post sem webhook. Precisa ver o
// evento agendado (senão o aviso "ainda não começou" nunca sai) e o encerrado
// há menos de 7 dias (N9/RN-37, limite do private reply do Instagram).
func TestPollingEnxergaAgendadoEEncerradoRecente(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		slug        string
		eventStatus string
		sessionType string
		endedDays   float64
		want        bool
	}{
		{"agendado entra", "poll-scheduled", "scheduled", SessionTypePost, 0, true},
		{"ativo entra", "poll-active", "active", SessionTypePost, 0, true},
		{"reel entra", "poll-reel", "active", SessionTypeReel, 0, true},
		{"encerrado ha 1 dia entra", "poll-ended-1d", "ended", SessionTypePost, 1, true},
		{"encerrado ha 6 dias entra", "poll-ended-6d", "ended", SessionTypePost, 6, true},
		{"encerrado ha 8 dias sai", "poll-ended-8d", "ended", SessionTypePost, 8, false},
		{"story nunca entra", "poll-story", "active", SessionTypeStory, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedMedia(t, ctx, tc.slug, tc.eventStatus, SessionStatusActive, tc.sessionType, tc.endedDays)

			medias, err := testRepo.ListPollableMedia(ctx)
			if err != nil {
				t.Fatalf("ListPollableMedia: %v", err)
			}
			found := false
			for _, m := range medias {
				if m.MediaID == f.mediaID {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("midia no polling = %v, quero %v", found, tc.want)
			}
		})
	}
}

// A5/D22 — com a mídia reaproveitável, o mesmo platform_live_id passa a ter N
// linhas e o `:one` do sqlc escolheria uma em silêncio. A resolução tem de
// preferir a campanha VIVA, e sessão e evento têm de sair da MESMA linha: são
// duas queries independentes e o comentário é gravado com o session_id de uma e
// o event_id da outra.
func TestResolucaoPrefereCampanhaVivaECoerente(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const mediaID = "media-reuso"

	antiga := seedMedia(t, ctx, "reuso-antiga", "ended", SessionStatusEnded, SessionTypePost, 1)
	// A mídia da campanha antiga foi liberada pelo trigger da 000116; a nova
	// campanha toma o mesmo media id.
	nova := seedMedia(t, ctx, "reuso-nova", "active", SessionStatusActive, SessionTypePost, 0)
	if _, err := testPool.Exec(ctx,
		`UPDATE live_session_platforms SET platform_live_id = $1 WHERE platform_live_id IN ($2,$3)`,
		mediaID, antiga.mediaID, nova.mediaID,
	); err != nil {
		t.Fatalf("reuso de midia barrado — a 000117 deveria permiti-lo: %v", err)
	}

	event, err := testRepo.GetEventByPlatformLiveID(ctx, mediaID)
	if err != nil || event == nil {
		t.Fatalf("GetEventByPlatformLiveID: %v (evento %v)", err, event)
	}
	if event.ID != nova.eventID {
		t.Errorf("resolveu a campanha %s, queria a VIVA %s", event.ID, nova.eventID)
	}

	session, err := testRepo.GetSessionByPlatformLiveID(ctx, mediaID)
	if err != nil || session == nil {
		t.Fatalf("GetSessionByPlatformLiveID: %v (sessao %v)", err, session)
	}
	if session.EventID != event.ID {
		t.Errorf("sessao veio do evento %s e o evento resolvido foi %s — o comentario ficaria com a sessao de uma campanha e o evento de outra",
			session.EventID, event.ID)
	}

	// A TERCEIRA resolução pela mesma chave. É a que roteia "a mídia sumiu do
	// Instagram" para o End do evento — ou seja, ela decide QUAL campanha vai
	// ser encerrada, com os carrinhos finalizados e o ERP reconciliado junto.
	// Elegendo a linha errada, o reuso de mídia encerraria a campanha VIVA
	// porque uma publicação antiga foi apagada.
	timed, err := testRepo.GetActiveTimedEventByMediaID(ctx, mediaID)
	if err != nil || timed == nil {
		t.Fatalf("GetActiveTimedEventByMediaID: %v (ref %v)", err, timed)
	}
	if timed.EventID != nova.eventID {
		t.Errorf("a resolução de mídia apagada elegeu %s; as outras duas elegeram %s — encerraria a campanha errada",
			timed.EventID, nova.eventID)
	}
	if timed.StoreID != nova.storeID {
		t.Errorf("store_id %s nao e o da campanha resolvida (%s)", timed.StoreID, nova.storeID)
	}
}

// webhook_active continua sendo o desligamento do polling — por MÍDIA.
func TestPollingParaQuandoOWebhookChega(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedMedia(t, ctx, "poll-webhook", "active", SessionStatusActive, SessionTypePost, 0)

	if err := testRepo.MarkMediaWebhookActive(ctx, f.mediaID); err != nil {
		t.Fatalf("MarkMediaWebhookActive: %v", err)
	}
	medias, err := testRepo.ListPollableMedia(ctx)
	if err != nil {
		t.Fatalf("ListPollableMedia: %v", err)
	}
	for _, m := range medias {
		if m.MediaID == f.mediaID {
			t.Fatal("midia com webhook ativo continuou no polling — captura duplicada e custo de API a toa")
		}
	}
}
