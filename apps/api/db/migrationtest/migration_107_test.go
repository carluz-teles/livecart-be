package migrationtest

// Teste da migration 000107: order_items ganha session_id, e com ele a
// CARDINALIDADE muda — o pedido passa a ter uma linha por (produto, sessão).
//
// Ela e a 000108 eram as únicas da leva 104-115 sem teste de schema, e são
// justamente as duas em que a métrica em dois níveis se apoia.
//
// O que precisa ficar provado:
//  1. a coluna existe, é nullable e referencia live_sessions;
//  2. NÃO existe unique em (order_id, product_id) — se existisse, a
//     cardinalidade nova seria impossível e o selamento estouraria em produção
//     no primeiro pedido com o mesmo produto vindo de duas transmissões;
//  3. o índice de leitura por sessão existe (é o que sustenta "quanto a live de
//     terça faturou");
//  4. apagar a sessão NÃO apaga a linha do pedido — ON DELETE SET NULL. Um
//     pedido pago não pode perder receita porque alguém removeu a transmissão;
//     ele cai no balde "sem transmissão";
//  5. o down desfaz.

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000107OrderItemsSessionSplit(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(107); err != nil {
		t.Fatalf("migrate ate 107: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	// 1. A coluna.
	var isNullable, dataType string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable, data_type FROM information_schema.columns
		 WHERE table_name = 'order_items' AND column_name = 'session_id'`,
	).Scan(&isNullable, &dataType); err != nil {
		t.Fatalf("order_items.session_id nao existe: %v", err)
	}
	if isNullable != "YES" {
		t.Errorf("session_id deveria ser nullable (adição pelo painel não tem sessão), got %q", isNullable)
	}
	if dataType != "uuid" {
		t.Errorf("session_id data_type = %q, quero uuid", dataType)
	}

	// 2. Nenhuma unique em (order_id, product_id).
	var uniques int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'order_items'
		   AND indexdef ILIKE '%UNIQUE%'
		   AND indexdef ILIKE '%order_id%'
		   AND indexdef ILIKE '%product_id%'`,
	).Scan(&uniques); err != nil {
		t.Fatalf("checar uniques: %v", err)
	}
	if uniques != 0 {
		t.Errorf("existe unique (order_id, product_id): a linha por (produto, sessao) seria impossivel")
	}

	// 3. O índice de leitura por sessão.
	var idx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'order_items' AND indexname = 'idx_order_items_session'`,
	).Scan(&idx); err != nil {
		t.Fatalf("checar indice: %v", err)
	}
	if idx != 1 {
		t.Error("idx_order_items_session ausente — a metrica por transmissao varreria a tabela")
	}

	// 4. ON DELETE SET NULL, provado com dado.
	var storeID, eventID, sessionID, productID, cartID, orderID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Split 107','split-107') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'ended','Semana') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_sessions (event_id, status, sequence_order) VALUES ($1,'ended',1) RETURNING id::text`,
		eventID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Vestido','none','ext-107','1070',2500,100) RETURNING id::text`,
		storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, paid_at)
		 VALUES ($1,'u107','@u107','tok107',1070,'checkout','paid',now()) RETURNING id::text`,
		eventID,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status, total_cents, discount_cents, shipping_cents, paid_total_cents, paid_at)
		 VALUES ($1,10700,$2,$3,'paid',5000,0,0,5000,now()) RETURNING id::text`,
		cartID, storeID, eventID,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// Duas linhas do MESMO produto, uma por transmissão — o caso que a 000107
	// existe para permitir. Uma delas sem sessão.
	if _, err := pool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price, session_id)
		 VALUES ($1,$2,'Vestido',1,2500,$3), ($1,$2,'Vestido',1,2500,NULL)`,
		orderID, productID, sessionID,
	); err != nil {
		t.Fatalf("o mesmo produto em duas linhas foi recusado: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM live_sessions WHERE id = $1::uuid`, sessionID); err != nil {
		t.Fatalf("apagar sessao: %v", err)
	}
	var linhas, semSessao int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE session_id IS NULL)
		 FROM order_items WHERE order_id = $1::uuid`, orderID,
	).Scan(&linhas, &semSessao); err != nil {
		t.Fatalf("reler order_items: %v", err)
	}
	if linhas != 2 {
		t.Errorf("apagar a sessao levou linha de pedido junto: sobraram %d de 2", linhas)
	}
	if semSessao != 2 {
		t.Errorf("session_id nao virou NULL no delete: %d de 2 sem sessao", semSessao)
	}

	// 5. Down.
	if err := m.Migrate(106); err != nil {
		t.Fatalf("down para 106: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'order_items' AND column_name = 'session_id'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar coluna apos down: %v", err)
	}
	if n != 0 {
		t.Error("down deixou order_items.session_id para tras")
	}
}
