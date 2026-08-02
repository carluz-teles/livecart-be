package migrationtest

// Teste da migration 000110: event_products vira session_products.
//
// O que precisa ficar provado:
//  1. a whitelist do evento é copiada para TODAS as sessões dele — sem isso,
//     pela semântica nova ("vazia = libera tudo"), toda barreira configurada
//     sumiria em silêncio no dia do deploy;
//  2. evento SEM sessão perde a lista (estado alcançável hoje: existe caminho
//     de criação que registra "live created without session"). É perda
//     consciente, e o teste a documenta;
//  3. a UNIQUE (session_id, product_id) segura duplicata;
//  4. event_products continua intacta (expand — o DROP é a 000119);
//  5. o down descarta só a cópia.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000110SessionProducts(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(109); err != nil {
		t.Fatalf("migrate ate 109: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Whitelist Test','wl-110') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	var productA, productB string
	for i, target := range []*string{&productA, &productB} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
			 VALUES ($1, $2, $3, 'manual', 1000, 10, true) RETURNING id::text`,
			storeID, []string{"Vestido", "Bolsa"}[i], []string{"VST1", "BLS1"}[i],
		).Scan(target); err != nil {
			t.Fatalf("seed produto %d: %v", i, err)
		}
	}

	// Evento com DUAS sessões e whitelist de dois produtos.
	var eventTwo string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,'active','Duas sessoes','multi') RETURNING id::text`,
		storeID,
	).Scan(&eventTwo); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	for seq := 1; seq <= 2; seq++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'active',$2)`,
			eventTwo, seq,
		); err != nil {
			t.Fatalf("seed sessao %d: %v", seq, err)
		}
	}
	for _, p := range []string{productA, productB} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO event_products (event_id, product_id, special_price, max_quantity, display_order, featured)
			 VALUES ($1,$2,800,3,0,true)`, eventTwo, p,
		); err != nil {
			t.Fatalf("seed event_product: %v", err)
		}
	}

	// Evento SEM sessão nenhuma, mas com whitelist.
	var eventOrphan string
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,'active','Sem sessao','single') RETURNING id::text`,
		storeID,
	).Scan(&eventOrphan); err != nil {
		t.Fatalf("seed evento orfao: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO event_products (event_id, product_id) VALUES ($1,$2)`, eventOrphan, productA,
	); err != nil {
		t.Fatalf("seed event_product orfao: %v", err)
	}

	// --- aplica a 000110 ---------------------------------------------------
	if err := m.Migrate(110); err != nil {
		t.Fatalf("migrate ate 110: %v", err)
	}

	// 1. Duas sessões × dois produtos = quatro linhas, com os valores preservados.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_products sp
		 JOIN live_sessions ls ON ls.id = sp.session_id
		 WHERE ls.event_id = $1::uuid`, eventTwo,
	).Scan(&n); err != nil {
		t.Fatalf("contar copia: %v", err)
	}
	if n != 4 {
		t.Errorf("whitelist copiada para %d linhas, quero 4 (2 sessoes x 2 produtos)", n)
	}

	var specialPrice, maxQuantity int
	var featured bool
	if err := pool.QueryRow(ctx,
		`SELECT sp.special_price, sp.max_quantity, sp.featured
		 FROM session_products sp
		 JOIN live_sessions ls ON ls.id = sp.session_id
		 WHERE ls.event_id = $1::uuid AND sp.product_id = $2::uuid
		 LIMIT 1`, eventTwo, productA,
	).Scan(&specialPrice, &maxQuantity, &featured); err != nil {
		t.Fatalf("ler copia: %v", err)
	}
	if specialPrice != 800 || maxQuantity != 3 || !featured {
		t.Errorf("valores nao vieram junto: special=%d max=%d featured=%v", specialPrice, maxQuantity, featured)
	}

	// 2. Evento sem sessão perde a lista — e, pela semântica nova, passa a
	//    vender tudo. Perda consciente, registrada aqui.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_products sp
		 JOIN live_sessions ls ON ls.id = sp.session_id
		 WHERE ls.event_id = $1::uuid`, eventOrphan,
	).Scan(&n); err != nil {
		t.Fatalf("contar orfao: %v", err)
	}
	if n != 0 {
		t.Errorf("evento sem sessao gerou %d linha(s) — o backfill precisa de sessao para ancorar", n)
	}

	// 3. UNIQUE (session_id, product_id).
	var anySession string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM live_sessions WHERE event_id = $1::uuid ORDER BY sequence_order LIMIT 1`, eventTwo,
	).Scan(&anySession); err != nil {
		t.Fatalf("ler sessao: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_products (session_id, product_id) VALUES ($1::uuid, $2::uuid)`, anySession, productA,
	); err == nil {
		t.Error("a UNIQUE (session_id, product_id) deixou passar duplicata")
	}

	// 4. EXPAND: event_products intacta.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_products`).Scan(&n); err != nil {
		t.Fatalf("contar event_products: %v", err)
	}
	if n != 3 {
		t.Errorf("event_products tem %d linhas, quero 3 — a origem tem de sobreviver ate a 000119", n)
	}

	// 5. Down.
	if err := m.Migrate(109); err != nil {
		t.Fatalf("down para 109: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'session_products'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar tabela apos down: %v", err)
	}
	if n != 0 {
		t.Error("down deixou session_products para tras")
	}
}
