package migrationtest

// Teste da migration 000114: released_at + os dois triggers que a mantem.
//
// O trigger E CODIGO, e codigo sem teste e suposicao. O que precisa ficar
// provado — e cada item corresponde a um caminho real de encerramento que o Go
// NAO cobriria se a coluna fosse mantida em EndLiveEvent:
//  1. backfill libera a midia de evento ja encerrado antes da migration;
//  2. encerrar pelo caminho do sqlc (EndLiveEvent / botao manual) libera;
//  3. encerrar por UPDATE cru — o que EndEventByMediaID faz, fora do sqlc, e o
//     que um incidente faz na mao — libera igual;
//  4. reabrir a campanha volta a midia para "em uso";
//  5. midia INSERIDA numa sessao de evento ja encerrado nasce liberada;
//  6. transicao de status que nao envolve 'ended' nao mexe em nada;
//  7. o down remove coluna, triggers e funcoes.

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000114LspReleasedAt(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(113); err != nil {
		t.Fatalf("migrate ate 113: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Midia','midia-114') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// seedEvent cria evento + sessao + midia e devolve (eventID, sessionID).
	seedEvent := func(title, status, mediaID string) (string, string) {
		t.Helper()
		var eventID, sessionID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,$2,$3,'post') RETURNING id::text`,
			storeID, status, title,
		).Scan(&eventID); err != nil {
			t.Fatalf("seed evento %s: %v", title, err)
		}
		var endedAt any
		if status == "ended" {
			endedAt = time.Now()
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_sessions (event_id, status, type, sequence_order, ended_at)
			 VALUES ($1,$2,'post',1,$3) RETURNING id::text`,
			eventID, status, endedAt,
		).Scan(&sessionID); err != nil {
			t.Fatalf("seed sessao %s: %v", title, err)
		}
		if mediaID != "" {
			if _, err := pool.Exec(ctx,
				`INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
				 VALUES ($1,'instagram',$2)`, sessionID, mediaID,
			); err != nil {
				t.Fatalf("seed midia %s: %v", mediaID, err)
			}
		}
		return eventID, sessionID
	}

	released := func(mediaID string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM live_session_platforms WHERE platform_live_id = $1 AND released_at IS NOT NULL`,
			mediaID,
		).Scan(&n); err != nil {
			t.Fatalf("ler released_at de %s: %v", mediaID, err)
		}
		return n == 1
	}

	// Legado: uma campanha ja encerrada e uma viva, ambas com midia.
	endedID, _ := seedEvent("Encerrada", "ended", "media-legado")
	seedEvent("Viva", "active", "media-viva")

	// --- aplica a 000114 ---------------------------------------------------
	if err := m.Migrate(114); err != nil {
		t.Fatalf("migrate ate 114: %v", err)
	}

	// 1: backfill.
	if !released("media-legado") {
		t.Error("backfill nao liberou a midia de evento ja encerrado — ela ficaria presa e o reuso nunca aconteceria")
	}
	if released("media-viva") {
		t.Error("backfill liberou midia de evento VIVO")
	}

	// 2: encerrar pelo caminho do sqlc (o mesmo UPDATE que EndLiveEvent faz).
	sqlcEventID, _ := seedEvent("Botao", "active", "media-botao")
	if _, err := pool.Exec(ctx,
		`UPDATE live_events SET status = 'ended', updated_at = now() WHERE id = $1::uuid`, sqlcEventID,
	); err != nil {
		t.Fatalf("encerrar pelo sqlc: %v", err)
	}
	if !released("media-botao") {
		t.Error("encerramento pelo caminho do sqlc nao liberou a midia")
	}

	// 3: encerrar por UPDATE cru com WHERE por EXISTS — a forma exata do
	// EndEventByMediaID, que vive fora do sqlc e por isso nunca seria coberto
	// se released_at fosse mantido no Go.
	seedEvent("MidiaSumiu", "active", "media-sumiu")
	if _, err := pool.Exec(ctx, `
		UPDATE live_events e
		SET status = 'ended', updated_at = now()
		WHERE e.status <> 'ended'
		  AND EXISTS (
		      SELECT 1 FROM live_sessions ls
		      JOIN live_session_platforms lsp ON lsp.session_id = ls.id
		      WHERE ls.event_id = e.id AND lsp.platform_live_id = 'media-sumiu')`,
	); err != nil {
		t.Fatalf("encerrar por UPDATE cru: %v", err)
	}
	if !released("media-sumiu") {
		t.Error("encerramento em SQL cru nao liberou a midia — e o caminho que o codigo Go nao cobriria")
	}

	// 4: reabrir devolve a midia para "em uso".
	if _, err := pool.Exec(ctx,
		`UPDATE live_events SET status = 'active' WHERE id = $1::uuid`, endedID,
	); err != nil {
		t.Fatalf("reabrir: %v", err)
	}
	if released("media-legado") {
		t.Error("campanha reaberta deixou a midia liberada — outra campanha poderia toma-la")
	}

	// 5: midia vinculada a uma sessao de evento ja encerrado nasce liberada.
	_, encerradoSessionID := seedEvent("Reparo", "ended", "")
	if _, err := pool.Exec(ctx,
		`INSERT INTO live_session_platforms (session_id, platform, platform_live_id)
		 VALUES ($1,'instagram','media-tardia')`, encerradoSessionID,
	); err != nil {
		t.Fatalf("inserir midia tardia: %v", err)
	}
	if !released("media-tardia") {
		t.Error("midia inserida em evento ja encerrado nasceu 'em uso' — o trigger de UPDATE nao a alcanca")
	}

	// 6: transicao que nao envolve 'ended' nao mexe em nada.
	naoEndedID, _ := seedEvent("Agendada", "scheduled", "media-agendada")
	if _, err := pool.Exec(ctx,
		`UPDATE live_events SET status = 'active' WHERE id = $1::uuid`, naoEndedID,
	); err != nil {
		t.Fatalf("scheduled -> active: %v", err)
	}
	if released("media-agendada") {
		t.Error("transicao scheduled->active liberou midia")
	}

	// 7: down limpa coluna, triggers e funcoes.
	if err := m.Migrate(113); err != nil {
		t.Fatalf("down para 113: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		         WHERE table_name = 'live_session_platforms' AND column_name = 'released_at')
		     + (SELECT count(*) FROM pg_trigger
		         WHERE tgname IN ('trg_sync_lsp_released_at','trg_set_lsp_released_at_on_insert'))
		     + (SELECT count(*) FROM pg_proc
		         WHERE proname IN ('sync_lsp_released_at','set_lsp_released_at_on_insert'))`,
	).Scan(&n); err != nil {
		t.Fatalf("checar residuo apos down: %v", err)
	}
	if n != 0 {
		t.Errorf("down deixou %d objetos para tras (coluna/trigger/funcao)", n)
	}
}
