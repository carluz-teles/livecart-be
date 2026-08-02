package migrationtest

// Teste da migration 000109: o TIPO e a MÍDIA descem do evento para a
// sessão/mídia.
//
// O que precisa ficar provado:
//  1. o vocabulário do evento (single|multi|post|story) é traduzido para o da
//     sessão (live|post|story) — 'single' e 'multi' são ambos live;
//  2. o CHECK novo REJEITA o vocabulário antigo em live_sessions.type — é a
//     armadilha da errata E6: validação de aplicação desalinhada do CHECK vira
//     500 no INSERT em vez de 422;
//  3. 'reel' passa a ser um valor legal (não existia em lugar nenhum antes);
//  4. legenda/permalink/thumb migram para a linha de mídia casando
//     platform_live_id = live_events.media_id, e webhook_active vem junto;
//  5. live_events continua intacto (expand — o DROP é a 000119);
//  6. o down desfaz tudo.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000109SessionTypeAndMedia(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(108); err != nil {
		t.Fatalf("migrate ate 108: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Tipo Test','tipo-109') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// Um evento de cada tipo do vocabulário antigo, cada um com uma sessão e uma
	// mídia. Só o de post carrega os metadados de mídia (é assim na produção:
	// media_* só é escrito por CreatePostEvent).
	type seed struct {
		eventType string
		mediaID   string
		wantType  string
	}
	seeds := []seed{
		{"single", "media-single-109", "live"},
		{"multi", "media-multi-109", "live"},
		{"post", "media-post-109", "post"},
		{"story", "media-story-109", "story"},
	}
	sessionIDs := map[string]string{}
	for _, sd := range seeds {
		var eventID, sessionID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_events (store_id, status, title, type, media_id, media_permalink, media_thumbnail_url, media_caption, webhook_active)
			 VALUES ($1,'active',$2,$2,$3,$4,$5,$6,true) RETURNING id::text`,
			storeID, sd.eventType, sd.mediaID,
			"https://instagram.com/p/"+sd.mediaID,
			"https://cdn/"+sd.mediaID+".jpg",
			"legenda de "+sd.mediaID,
		).Scan(&eventID); err != nil {
			t.Fatalf("seed evento %s: %v", sd.eventType, err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'active',1) RETURNING id::text`,
			eventID,
		).Scan(&sessionID); err != nil {
			t.Fatalf("seed sessao %s: %v", sd.eventType, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO live_session_platforms (session_id, platform, platform_live_id) VALUES ($1,'instagram',$2)`,
			sessionID, sd.mediaID,
		); err != nil {
			t.Fatalf("seed midia %s: %v", sd.eventType, err)
		}
		sessionIDs[sd.eventType] = sessionID
	}

	// --- aplica a 000109 ---------------------------------------------------
	if err := m.Migrate(109); err != nil {
		t.Fatalf("migrate ate 109: %v", err)
	}

	// 1. Tradução do vocabulário.
	for _, sd := range seeds {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT type FROM live_sessions WHERE id = $1::uuid`, sessionIDs[sd.eventType],
		).Scan(&got); err != nil {
			t.Fatalf("ler tipo da sessao %s: %v", sd.eventType, err)
		}
		if got != sd.wantType {
			t.Errorf("evento type=%q: sessao ficou %q, quero %q", sd.eventType, got, sd.wantType)
		}
	}

	// 2. O CHECK rejeita o vocabulário ANTIGO — se a aplicação continuar
	//    mandando 'single'/'multi' para live_sessions, estoura como 500.
	for _, bad := range []string{"single", "multi", "", "video"} {
		if _, err := pool.Exec(ctx,
			`UPDATE live_sessions SET type = $2 WHERE id = $1::uuid`, sessionIDs["single"], bad,
		); err == nil {
			t.Errorf("o CHECK deixou passar type=%q em live_sessions", bad)
		}
	}

	// 3. 'reel' é valor legal agora (não existia no vocabulário do evento).
	if _, err := pool.Exec(ctx,
		`UPDATE live_sessions SET type = 'reel' WHERE id = $1::uuid`, sessionIDs["post"],
	); err != nil {
		t.Errorf("'reel' devia ser aceito em live_sessions.type: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE live_sessions SET type = 'post' WHERE id = $1::uuid`, sessionIDs["post"],
	); err != nil {
		t.Fatalf("restaurar type=post: %v", err)
	}

	// 4. Metadados de mídia migraram casando platform_live_id = media_id.
	var permalink, thumb, caption string
	var webhookActive bool
	if err := pool.QueryRow(ctx,
		`SELECT media_permalink, media_thumbnail_url, media_caption, webhook_active
		 FROM live_session_platforms WHERE platform_live_id = 'media-post-109'`,
	).Scan(&permalink, &thumb, &caption, &webhookActive); err != nil {
		t.Fatalf("ler metadados da midia: %v", err)
	}
	if permalink != "https://instagram.com/p/media-post-109" {
		t.Errorf("permalink = %q", permalink)
	}
	if thumb != "https://cdn/media-post-109.jpg" {
		t.Errorf("thumbnail = %q", thumb)
	}
	if caption != "legenda de media-post-109" {
		t.Errorf("caption = %q", caption)
	}
	if !webhookActive {
		t.Error("webhook_active nao veio do evento — a midia nasceria em polling de novo")
	}

	// 5. EXPAND: live_events continua com tudo. Dropar aqui quebraria o FE.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'live_events'
		   AND column_name IN ('type','media_id','media_permalink','media_thumbnail_url','media_caption','webhook_active')`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas do evento: %v", err)
	}
	if n != 6 {
		t.Errorf("live_events perdeu colunas na 000109 (%d de 6 presentes) — o contract e a 000119", n)
	}

	// 6. Down.
	if err := m.Migrate(108); err != nil {
		t.Fatalf("down para 108: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE (table_name = 'live_sessions' AND column_name = 'type')
		    OR (table_name = 'live_session_platforms'
		        AND column_name IN ('media_permalink','media_thumbnail_url','media_caption','webhook_active'))`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas apos down: %v", err)
	}
	if n != 0 {
		t.Errorf("down deixou %d coluna(s) para tras", n)
	}
}
