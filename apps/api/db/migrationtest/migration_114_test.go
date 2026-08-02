package migrationtest

// Teste da migration 000114: janela comercial do evento (starts_at/ends_at) e
// agendamento de publicacao da sessao (publish_at).
//
// O que precisa ficar provado:
//  1. starts_at herda scheduled_at quando havia intencao explicita de inicio, e
//     fica NULL quando nao havia — inventar data mudaria rotulo ja visto;
//  2. evento ENCERRADO sem ends_at ganha o carimbo do fim (MAX(ended_at) das
//     sessoes, senao updated_at);
//  3. evento VIVO sem ends_at NAO e tocado: gravar ends_at nele o fecharia no
//     primeiro sweep;
//  4. ends_at continua NULLABLE e SEM CHECK — um CHECK aqui quebraria todo
//     UPDATE de evento legado, inclusive o do caminho de pagamento;
//  5. publish_at nasce na sessao;
//  6. o down desfaz o que e reversivel.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000114EventWindowAndPublishAt(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(113); err != nil {
		t.Fatalf("migrate ate 111: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Janela Test','win-112') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// (a) evento agendado: tem scheduled_at, deve virar starts_at.
	var scheduledEvent string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, scheduled_at)
		 VALUES ($1,'scheduled','Agendado','multi', now() + interval '2 days')
		 RETURNING id::text`, storeID,
	).Scan(&scheduledEvent); err != nil {
		t.Fatalf("seed evento agendado: %v", err)
	}

	// (b) evento vivo sem janela nenhuma: nao pode ganhar ends_at.
	var liveEvent string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type)
		 VALUES ($1,'active','Ao vivo','multi') RETURNING id::text`, storeID,
	).Scan(&liveEvent); err != nil {
		t.Fatalf("seed evento vivo: %v", err)
	}

	// (c) evento encerrado com sessao encerrada: ends_at = MAX(ended_at).
	var endedWithSession string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type)
		 VALUES ($1,'ended','Encerrado com sessao','multi') RETURNING id::text`, storeID,
	).Scan(&endedWithSession); err != nil {
		t.Fatalf("seed evento encerrado: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, ended_at)
		 VALUES ($1,'ended',1, now() - interval '3 days'),
		        ($1,'ended',2, now() - interval '1 day')`, endedWithSession,
	); err != nil {
		t.Fatalf("seed sessoes encerradas: %v", err)
	}

	// (d) evento encerrado SEM sessao: cai no updated_at.
	var endedNoSession string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type)
		 VALUES ($1,'ended','Encerrado sem sessao','multi') RETURNING id::text`, storeID,
	).Scan(&endedNoSession); err != nil {
		t.Fatalf("seed evento encerrado sem sessao: %v", err)
	}

	// --- aplica a 000114 ---------------------------------------------------
	if err := m.Migrate(114); err != nil {
		t.Fatalf("migrate ate 112: %v", err)
	}

	// 1. starts_at copiado de scheduled_at.
	var sameStart bool
	if err := pool.QueryRow(ctx,
		`SELECT starts_at IS NOT NULL AND starts_at = scheduled_at
		 FROM live_events WHERE id = $1::uuid`, scheduledEvent,
	).Scan(&sameStart); err != nil {
		t.Fatalf("ler starts_at do agendado: %v", err)
	}
	if !sameStart {
		t.Error("evento agendado nao herdou starts_at de scheduled_at")
	}

	// ... e NULL quando nao havia intencao.
	var startNull bool
	if err := pool.QueryRow(ctx,
		`SELECT starts_at IS NULL FROM live_events WHERE id = $1::uuid`, liveEvent,
	).Scan(&startNull); err != nil {
		t.Fatalf("ler starts_at do vivo: %v", err)
	}
	if !startNull {
		t.Error("evento sem scheduled_at ganhou starts_at inventado")
	}

	// 2. Encerrado com sessao: ends_at = maior ended_at das sessoes.
	var matchesMaxEnded bool
	if err := pool.QueryRow(ctx,
		`SELECT e.ends_at IS NOT NULL
		        AND e.ends_at = (SELECT MAX(ls.ended_at) FROM live_sessions ls WHERE ls.event_id = e.id)
		 FROM live_events e WHERE e.id = $1::uuid`, endedWithSession,
	).Scan(&matchesMaxEnded); err != nil {
		t.Fatalf("ler ends_at do encerrado: %v", err)
	}
	if !matchesMaxEnded {
		t.Error("evento encerrado nao recebeu ends_at = MAX(ended_at) das sessoes")
	}

	// ... e o sem sessao cai no updated_at.
	var matchesUpdated bool
	if err := pool.QueryRow(ctx,
		`SELECT ends_at IS NOT NULL AND ends_at = updated_at
		 FROM live_events WHERE id = $1::uuid`, endedNoSession,
	).Scan(&matchesUpdated); err != nil {
		t.Fatalf("ler ends_at do encerrado sem sessao: %v", err)
	}
	if !matchesUpdated {
		t.Error("evento encerrado sem sessao nao caiu no updated_at")
	}

	// 3. Evento VIVO nao foi tocado — este e o teste que impede o backfill de
	//    fechar campanha em andamento.
	var liveEndsAtNull bool
	if err := pool.QueryRow(ctx,
		`SELECT ends_at IS NULL FROM live_events WHERE id = $1::uuid`, liveEvent,
	).Scan(&liveEndsAtNull); err != nil {
		t.Fatalf("ler ends_at do vivo: %v", err)
	}
	if !liveEndsAtNull {
		t.Error("backfill gravou ends_at num evento ATIVO — ele fecharia no primeiro sweep")
	}

	// 4. ends_at segue nullable e sem CHECK: UPDATE em evento legado sem janela
	//    tem de continuar passando (o caminho de pagamento faz isso).
	if _, err := pool.Exec(ctx,
		`UPDATE live_events SET total_orders = total_orders + 1 WHERE id = $1::uuid`, liveEvent,
	); err != nil {
		t.Fatalf("UPDATE em evento sem ends_at falhou — a obrigatoriedade nao pode estar no banco ainda: %v", err)
	}

	// 5. publish_at existe na sessao e aceita NULL e valor.
	var publishSession string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, publish_at)
		 VALUES ($1,'scheduled',9, now() + interval '1 day') RETURNING id::text`, liveEvent,
	).Scan(&publishSession); err != nil {
		t.Fatalf("inserir sessao com publish_at: %v", err)
	}

	// 6. Down.
	if err := m.Migrate(113); err != nil {
		t.Fatalf("down para 111: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE (table_name = 'live_events'   AND column_name = 'starts_at')
		    OR (table_name = 'live_sessions' AND column_name = 'publish_at')`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas apos down: %v", err)
	}
	if n != 0 {
		t.Errorf("down deixou %d coluna(s) para tras", n)
	}
	// O ends_at dos encerrados fica de proposito (dado derivado e correto).
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM live_events WHERE status = 'ended' AND ends_at IS NOT NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("checar ends_at apos down: %v", err)
	}
	if n != 2 {
		t.Errorf("down apagou o ends_at dos encerrados (%d de 2 restantes)", n)
	}
}
