package migrationtest

// Teste da migration 000123: session_publish_jobs (RN-31 / N3).
//
// O que precisa ficar provado:
//  1. o job existe SEM sessao e SEM evento — e a divergencia deliberada do
//     plano (ele previa session_id NOT NULL). Se isso deixar de valer, nao ha
//     como agendar para D+3: a sessao so nasce com a midia publicada;
//  2. o CHECK de status recusa um sexto valor, e o de media_kind recusa um
//     quarto — sao os dois vocabularios que a task interpreta;
//  3. product_ids vazio e recusado: um agendamento sem produto publica um post
//     que nao vende nada, e a falha teria de aparecer so no disparo;
//  4. apagar o evento NAO apaga o job (SET NULL) — o rastro de que a
//     publicacao aconteceu sobrevive ao evento;
//  5. apagar a loja apaga o job (CASCADE) — job orfao dispararia publicacao em
//     nome de uma loja que nao existe mais;
//  6. os indices parciais existem com o predicado certo: sao eles que tornam o
//     backstop (varredura de vencidos e de presos) barato;
//  7. o down remove a tabela.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000123SessionPublishJobs(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(123); err != nil {
		t.Fatalf("migrate ate 121: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('N3','n3-121') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	var productID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1::uuid, 'Vestido', 'tiny', 'p121', 'V121', 10000, 5) RETURNING id::text`, storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	insertJob := func(status string) (string, error) {
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO session_publish_jobs
			   (store_id, media_kind, asset_path, asset_content_type, product_ids, scheduled_for, status)
			 VALUES ($1::uuid, 'post', 'instagram/x/a.jpg', 'image/jpeg', ARRAY[$2::uuid], now() + interval '3 days', $3)
			 RETURNING id::text`, storeID, productID, status,
		).Scan(&id)
		return id, err
	}

	// 1. Agendamento sem sessao e sem evento — o caso NORMAL, nao a exceção.
	jobID, err := insertJob("scheduled")
	if err != nil {
		t.Fatalf("job sem sessao devia ser aceito (a sessao so nasce com a midia publicada): %v", err)
	}

	// 2. Vocabulario fechado nas duas pontas.
	if _, err := insertJob("queued"); err == nil {
		t.Fatal("status fora do CHECK foi aceito — a task interpretaria um estado que nao existe")
	} else if !strings.Contains(err.Error(), "session_publish_jobs_status_check") {
		t.Fatalf("esperava violacao do CHECK de status, veio: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_publish_jobs
		   (store_id, media_kind, asset_path, asset_content_type, product_ids, scheduled_for)
		 VALUES ($1::uuid, 'carousel', 'k', 'image/jpeg', ARRAY[$2::uuid], now())`, storeID, productID,
	); err == nil {
		t.Fatal("media_kind fora do CHECK foi aceito — nao ha chamada da Graph correspondente")
	}

	// 3. Agendamento sem produto e publicacao que nao vende.
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_publish_jobs
		   (store_id, media_kind, asset_path, asset_content_type, product_ids, scheduled_for)
		 VALUES ($1::uuid, 'post', 'k', 'image/jpeg', ARRAY[]::uuid[], now())`, storeID,
	); err == nil {
		t.Fatal("product_ids vazio foi aceito — o post publicaria sem nada para vender")
	}

	// 4. O rastro sobrevive ao evento.
	var eventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1::uuid, 'active', 'Publicado', now() + interval '1 day') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	var sessionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order, type)
		 VALUES ($1::uuid, 'active', 0, 'post') RETURNING id::text`, eventID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seed sessao: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE session_publish_jobs
		    SET status='published', event_id=$2::uuid, session_id=$3::uuid, published_media_id='17900'
		  WHERE id=$1::uuid`, jobID, eventID, sessionID,
	); err != nil {
		t.Fatalf("marcar publicado: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM live_events WHERE id=$1::uuid`, eventID); err != nil {
		t.Fatalf("apagar evento: %v", err)
	}
	var status string
	var eventNull, sessionNull bool
	if err := pool.QueryRow(ctx,
		`SELECT status, event_id IS NULL, session_id IS NULL FROM session_publish_jobs WHERE id=$1::uuid`, jobID,
	).Scan(&status, &eventNull, &sessionNull); err != nil {
		t.Fatalf("job sumiu junto com o evento: %v", err)
	}
	if status != "published" || !eventNull || !sessionNull {
		t.Fatalf("esperava job preservado com os vinculos zerados, veio status=%q event_null=%v session_null=%v",
			status, eventNull, sessionNull)
	}

	// 5. Job orfao de loja nao pode existir. (products nao cascateia da loja —
	// e uma FK RESTRICT anterior ao epico —, entao o teste tira o produto do
	// caminho antes; product_ids e array sem FK, o job nao depende dele.)
	if _, err := pool.Exec(ctx, `DELETE FROM products WHERE store_id=$1::uuid`, storeID); err != nil {
		t.Fatalf("apagar produtos: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM stores WHERE id=$1::uuid`, storeID); err != nil {
		t.Fatalf("apagar loja: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_publish_jobs`).Scan(&remaining); err != nil {
		t.Fatalf("contar jobs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("esperava CASCADE da loja, sobraram %d jobs", remaining)
	}

	// 6. Os indices que tornam o backstop barato.
	for _, idx := range []struct{ name, predicate string }{
		{"idx_session_publish_jobs_due", "'scheduled'::text"},
		{"idx_session_publish_jobs_stuck", "'publishing'::text"},
	} {
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE tablename='session_publish_jobs' AND indexname=$1`, idx.name,
		).Scan(&def); err != nil {
			t.Fatalf("indice %s ausente: %v", idx.name, err)
		}
		if !strings.Contains(def, idx.predicate) {
			t.Fatalf("indice %s sem o predicado %q: %s", idx.name, idx.predicate, def)
		}
	}

	// 7. Down.
	if err := m.Migrate(122); err != nil {
		t.Fatalf("down para 120: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='session_publish_jobs')`,
	).Scan(&exists); err != nil {
		t.Fatalf("checar tabela: %v", err)
	}
	if exists {
		t.Fatal("a down deixou session_publish_jobs de pe")
	}
}
