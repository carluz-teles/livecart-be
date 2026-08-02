package migrationtest

// Teste da migration 000121: reparo 1:1 (D26) e marcador de corte (D12/D26).
//
// O que precisa ficar provado:
//  1. evento SEM sessao ganha exatamente uma, com type traduzido do
//     vocabulario do evento e status coerente com o do evento;
//  2. evento que JA tem sessao nao ganha nenhuma — o reparo nao pode duplicar
//     transmissao, senao a metrica por sessao nasce contando duas vezes;
//  3. o marcador global existe com a chave 'session_attribution' e e
//     idempotente (rodar de novo nao reescreve o instante do corte);
//  4. toda sessao que existia ANTES do corte fica marcada 'first_touch', e a
//     que nasce DEPOIS nasce 'addition_log' pelo DEFAULT — e isso que permite
//     a tela avisar em qual transmissao o numero mudou de definicao;
//  5. o CHECK recusa um terceiro valor;
//  6. o down remove tabela, coluna e constraint, mas MANTEM as sessoes do
//     reparo (apaga-las deixaria carrinho e comentario sem origem).

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000121AttributionCutover(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(120); err != nil {
		t.Fatalf("migrate ate 118: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('D26','d26-119') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	seedEvent := func(title, evType, status string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO live_events (store_id, status, title, type, ends_at)
			 VALUES ($1::uuid, $2, $3, $4, now()) RETURNING id::text`,
			storeID, status, title, evType,
		).Scan(&id); err != nil {
			t.Fatalf("seed evento %s: %v", title, err)
		}
		return id
	}

	// Orfaos de sessao, um por vocabulario do evento.
	orphanPost := seedEvent("Post orfao", "post", "ended")
	orphanStory := seedEvent("Story orfao", "story", "active")
	orphanMulti := seedEvent("Live orfa", "multi", "active")

	// Evento COM sessao: o reparo nao pode encostar nele.
	withSession := seedEvent("Ja tem sessao", "single", "active")
	var existingSessionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, type)
		 VALUES ($1::uuid, 'live', 1, 'live') RETURNING id::text`, withSession,
	).Scan(&existingSessionID); err != nil {
		t.Fatalf("seed sessao existente: %v", err)
	}

	if err := m.Migrate(121); err != nil {
		t.Fatalf("migrate 119: %v", err)
	}

	// 1: cada orfao ganhou UMA sessao, com o type traduzido.
	for _, tc := range []struct {
		eventID    string
		wantType   string
		wantStatus string
		label      string
	}{
		{orphanPost, "post", "ended", "post encerrado"},
		{orphanStory, "story", "active", "story ativo"},
		{orphanMulti, "live", "active", "multi vira live"},
	} {
		var n int
		var gotType, gotStatus string
		if err := pool.QueryRow(ctx,
			`SELECT count(*), max(type), max(status) FROM live_sessions WHERE event_id = $1::uuid`, tc.eventID,
		).Scan(&n, &gotType, &gotStatus); err != nil {
			t.Fatalf("%s: ler sessao: %v", tc.label, err)
		}
		if n != 1 {
			t.Errorf("%s: %d sessoes apos o reparo, quero 1", tc.label, n)
			continue
		}
		if gotType != tc.wantType {
			t.Errorf("%s: type = %q, quero %q", tc.label, gotType, tc.wantType)
		}
		if gotStatus != tc.wantStatus {
			t.Errorf("%s: status = %q, quero %q", tc.label, gotStatus, tc.wantStatus)
		}
	}

	// 2: o evento que ja tinha sessao continua com UMA, e e a mesma.
	var sessions int
	var stillThere string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(id::text) FROM live_sessions WHERE event_id = $1::uuid`, withSession,
	).Scan(&sessions, &stillThere); err != nil {
		t.Fatalf("ler sessoes do evento ja povoado: %v", err)
	}
	if sessions != 1 || stillThere != existingSessionID {
		t.Errorf("o reparo duplicou transmissao: %d sessoes, id %q (quero 1 e %q)", sessions, stillThere, existingSessionID)
	}

	// 3: o marcador global.
	var effectiveAt time.Time
	var note string
	if err := pool.QueryRow(ctx,
		`SELECT effective_at, note FROM metric_cutovers WHERE key = 'session_attribution'`,
	).Scan(&effectiveAt, &note); err != nil {
		t.Fatalf("marcador 'session_attribution' nao existe: %v", err)
	}
	if note == "" {
		t.Error("o marcador sem nota nao explica nada — a nota E o produto aqui")
	}

	// Idempotencia: reaplicar o INSERT nao pode mover o instante do corte, ou
	// o "antes" e o "depois" mudam de lugar a cada deploy.
	if _, err := pool.Exec(ctx,
		`INSERT INTO metric_cutovers (key, effective_at, note)
		 VALUES ('session_attribution', now(), 'nova tentativa') ON CONFLICT (key) DO NOTHING`,
	); err != nil {
		t.Fatalf("reinserir marcador: %v", err)
	}
	var again time.Time
	if err := pool.QueryRow(ctx,
		`SELECT effective_at FROM metric_cutovers WHERE key = 'session_attribution'`,
	).Scan(&again); err != nil {
		t.Fatalf("reler marcador: %v", err)
	}
	if !again.Equal(effectiveAt) {
		t.Errorf("o instante do corte mudou de %v para %v — o ON CONFLICT DO NOTHING nao esta protegendo", effectiveAt, again)
	}

	// 4: tudo que existia antes do corte esta marcado first_touch.
	var notMarked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM live_sessions WHERE attribution_source <> 'first_touch'`,
	).Scan(&notMarked); err != nil {
		t.Fatalf("contar sessoes marcadas: %v", err)
	}
	if notMarked != 0 {
		t.Errorf("%d sessoes anteriores ao corte ficaram sem a marca first_touch", notMarked)
	}

	// E a sessao nova nasce addition_log pelo DEFAULT, sem ninguem escrever.
	var born string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, type)
		 VALUES ($1::uuid, 'active', 2, 'post') RETURNING attribution_source`, withSession,
	).Scan(&born); err != nil {
		t.Fatalf("seed sessao pos-corte: %v", err)
	}
	if born != "addition_log" {
		t.Errorf("sessao criada depois do corte nasceu %q, quero addition_log", born)
	}

	// 5: o CHECK recusa valor fora do vocabulario.
	if _, err := pool.Exec(ctx,
		`UPDATE live_sessions SET attribution_source = 'chute' WHERE event_id = $1::uuid`, withSession,
	); err == nil {
		t.Error("o CHECK de attribution_source nao esta valendo")
	}

	// 6: down.
	if err := m.Migrate(120); err != nil {
		t.Fatalf("down para 118: %v", err)
	}
	var tbl, col int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'metric_cutovers'`,
	).Scan(&tbl); err != nil {
		t.Fatalf("checar tabela apos down: %v", err)
	}
	if tbl != 0 {
		t.Error("down deixou metric_cutovers para tras")
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'live_sessions' AND column_name = 'attribution_source'`,
	).Scan(&col); err != nil {
		t.Fatalf("checar coluna apos down: %v", err)
	}
	if col != 0 {
		t.Error("down deixou attribution_source para tras")
	}
	// As sessoes do reparo continuam la: e o comportamento declarado no down.
	var repaired int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM live_sessions WHERE event_id IN ($1::uuid, $2::uuid, $3::uuid)`,
		orphanPost, orphanStory, orphanMulti,
	).Scan(&repaired); err != nil {
		t.Fatalf("contar sessoes reparadas apos down: %v", err)
	}
	if repaired != 3 {
		t.Errorf("sessoes do reparo apos down = %d, quero 3 — apaga-las deixaria carrinho e comentario sem origem", repaired)
	}
}
