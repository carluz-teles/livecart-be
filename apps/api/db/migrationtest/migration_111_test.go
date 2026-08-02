package migrationtest

// Teste da migration 000111: o Modo Live (produto em destaque + pausa) desce
// para a sessão.
//
// O que precisa ficar provado:
//  1. sessão VIVA herda o estado do evento — senão o painel de uma live em
//     andamento perde o destaque no deploy;
//  2. sessão ENCERRADA não herda: copiar estado efêmero para lá ressuscitaria
//     um "produto em destaque" que já não existe;
//  3. a FK do produto é ON DELETE SET NULL (apagar o produto não derruba a
//     sessão);
//  4. live_events continua com as colunas (expand — o DROP é a 000119);
//  5. o down desfaz tudo.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000111SessionLiveMode(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(110); err != nil {
		t.Fatalf("migrate ate 110: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID, productID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Live Mode Test','lm-111') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Vestido','VST1','manual',1000,10,true) RETURNING id::text`, storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed produto: %v", err)
	}

	// Evento com destaque e processamento pausado, uma sessão viva e uma
	// encerrada.
	var eventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, current_active_product_id, processing_paused)
		 VALUES ($1,'active','Modo live','multi',$2,true) RETURNING id::text`, storeID, productID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	var liveSession, endedSession string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'live',1) RETURNING id::text`, eventID,
	).Scan(&liveSession); err != nil {
		t.Fatalf("seed sessao viva: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'ended',2) RETURNING id::text`, eventID,
	).Scan(&endedSession); err != nil {
		t.Fatalf("seed sessao encerrada: %v", err)
	}

	// --- aplica a 000111 ---------------------------------------------------
	if err := m.Migrate(111); err != nil {
		t.Fatalf("migrate ate 111: %v", err)
	}

	// 1. Sessão viva herdou.
	var gotProduct *string
	var gotPaused bool
	if err := pool.QueryRow(ctx,
		`SELECT current_active_product_id::text, processing_paused FROM live_sessions WHERE id = $1::uuid`, liveSession,
	).Scan(&gotProduct, &gotPaused); err != nil {
		t.Fatalf("ler sessao viva: %v", err)
	}
	if gotProduct == nil || *gotProduct != productID {
		t.Errorf("sessao viva nao herdou o produto em destaque: %v", gotProduct)
	}
	if !gotPaused {
		t.Error("sessao viva nao herdou a pausa — o processamento voltaria sozinho no deploy")
	}

	// 2. Sessão encerrada NÃO herdou.
	if err := pool.QueryRow(ctx,
		`SELECT current_active_product_id::text, processing_paused FROM live_sessions WHERE id = $1::uuid`, endedSession,
	).Scan(&gotProduct, &gotPaused); err != nil {
		t.Fatalf("ler sessao encerrada: %v", err)
	}
	if gotProduct != nil {
		t.Errorf("sessao encerrada herdou destaque (%v) — estado efemero nao ressuscita", *gotProduct)
	}
	if gotPaused {
		t.Error("sessao encerrada herdou a pausa")
	}

	// 3. FK ON DELETE SET NULL.
	if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id = $1::uuid`, productID); err != nil {
		t.Fatalf("apagar produto: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT current_active_product_id::text FROM live_sessions WHERE id = $1::uuid`, liveSession,
	).Scan(&gotProduct); err != nil {
		t.Fatalf("reler sessao viva: %v", err)
	}
	if gotProduct != nil {
		t.Errorf("apagar o produto devia zerar o destaque, veio %v", *gotProduct)
	}

	// 4. EXPAND: as colunas do evento continuam lá.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'live_events'
		   AND column_name IN ('current_active_product_id','processing_paused')`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas do evento: %v", err)
	}
	if n != 2 {
		t.Errorf("live_events perdeu colunas na 000111 (%d de 2) — o contract e a 000119", n)
	}

	// 5. Down.
	if err := m.Migrate(110); err != nil {
		t.Fatalf("down para 110: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'live_sessions'
		   AND column_name IN ('current_active_product_id','processing_paused')`,
	).Scan(&n); err != nil {
		t.Fatalf("checar colunas apos down: %v", err)
	}
	if n != 0 {
		t.Errorf("down deixou %d coluna(s) para tras", n)
	}
}
