package migrationtest

// Teste da migration 000108: o log de adições ao carrinho.
//
// O que precisa ficar provado:
//  1. a tabela existe com as colunas que a alocação consome;
//  2. quantity > 0 é CHECK — remoção NÃO é linha negativa. Se o banco aceitasse
//     quantidade negativa, alguém "corrigiria" uma remoção assim e o alocador,
//     que só soma adições, passaria a mentir sem erro nenhum;
//  3. session_id é nullable (adição pelo painel não tem transmissão);
//  4. apagar a sessão preserva a linha e zera a sessão (SET NULL); apagar o
//     CARRINHO leva o log junto (CASCADE) — o log não sobrevive ao seu dono;
//  5. os dois índices existem: o do selamento (cart, produto, cronologia) e o
//     da métrica (sessão);
//  6. o down desfaz.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000108CartItemEvents(t *testing.T) {
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

	// 1. Colunas.
	var cols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'cart_item_events'
		   AND column_name IN ('id','cart_id','product_id','session_id','quantity','unit_price','created_at')`,
	).Scan(&cols); err != nil {
		t.Fatalf("checar colunas: %v", err)
	}
	if cols != 7 {
		t.Fatalf("cart_item_events tem %d das 7 colunas esperadas", cols)
	}

	// 3. session_id nullable.
	var isNullable string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'cart_item_events' AND column_name = 'session_id'`,
	).Scan(&isNullable); err != nil {
		t.Fatalf("checar nullability: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("session_id deveria ser nullable, got %q", isNullable)
	}

	// 5. Índices.
	for _, name := range []string{"idx_cart_item_events_cart_product", "idx_cart_item_events_session"} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE tablename = 'cart_item_events' AND indexname = $1`, name,
		).Scan(&n); err != nil {
			t.Fatalf("checar indice %s: %v", name, err)
		}
		if n != 1 {
			t.Errorf("indice %s ausente", name)
		}
	}

	// Seed para os testes de comportamento.
	var storeID, eventID, sessionID, productID, cartID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Log 108','log-108') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'active','Semana') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'active',1) RETURNING id::text`,
		eventID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Vestido','none','ext-108','1080',2500,100) RETURNING id::text`,
		storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status)
		 VALUES ($1,'u108','@u108','tok108',1080,'active') RETURNING id::text`,
		eventID,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	// 2. Remoção não é linha negativa — nem zero.
	for _, bad := range []int{0, -1} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
			 VALUES ($1,$2,$3,$4,2500)`,
			cartID, productID, sessionID, bad,
		); err == nil {
			t.Errorf("o CHECK deixou passar quantity=%d — o alocador so soma adicoes", bad)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
		 VALUES ($1,$2,$3,1,-1)`,
		cartID, productID, sessionID,
	); err == nil {
		t.Error("o CHECK deixou passar unit_price negativo")
	}

	// Duas adições legítimas: uma com sessão, uma sem.
	if _, err := pool.Exec(ctx,
		`INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
		 VALUES ($1,$2,$3,1,2500), ($1,$2,NULL,1,2500)`,
		cartID, productID, sessionID,
	); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	// 4a. Apagar a sessão preserva o log.
	if _, err := pool.Exec(ctx, `DELETE FROM live_sessions WHERE id = $1::uuid`, sessionID); err != nil {
		t.Fatalf("apagar sessao: %v", err)
	}
	var linhas, semSessao int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE session_id IS NULL)
		 FROM cart_item_events WHERE cart_id = $1::uuid`, cartID,
	).Scan(&linhas, &semSessao); err != nil {
		t.Fatalf("reler log: %v", err)
	}
	if linhas != 2 || semSessao != 2 {
		t.Errorf("apagar a sessao devia zerar session_id e manter as 2 linhas; got linhas=%d semSessao=%d",
			linhas, semSessao)
	}

	// 4b. Apagar o carrinho leva o log junto.
	if _, err := pool.Exec(ctx, `DELETE FROM carts WHERE id = $1::uuid`, cartID); err != nil {
		t.Fatalf("apagar cart: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM cart_item_events WHERE cart_id = $1::uuid`, cartID,
	).Scan(&linhas); err != nil {
		t.Fatalf("reler log apos delete do cart: %v", err)
	}
	if linhas != 0 {
		t.Errorf("log sobreviveu ao carrinho: %d linha(s) orfa(s)", linhas)
	}

	// 6. Down.
	if err := m.Migrate(107); err != nil {
		t.Fatalf("down para 107: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'cart_item_events'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar tabela apos down: %v", err)
	}
	if n != 0 {
		t.Error("down deixou cart_item_events para tras")
	}
}
