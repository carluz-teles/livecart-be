package migrationtest

// Teste da migration 000113: a fila do evento não admite duplicidade viva.
//
// O que precisa ficar provado:
//  1. duplicata VIVA é colapsada mantendo a MELHOR posição (menor position);
//  2. a entrada perdedora vira 'cancelled' com cancelled_at — é o dado que o
//     down NÃO devolve;
//  3. o índice barra uma segunda entrada viva do mesmo comprador;
//  4. entradas expired/fulfilled/cancelled ficam FORA do predicado, então
//     voltar para a fila continua permitido (o predicado errado proibiria);
//  5. o mesmo comprador pode estar na fila de OUTRO produto e de OUTRO evento;
//  6. o down remove o índice.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000113WaitlistLiveEntryUnique(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	if err := m.Migrate(112); err != nil {
		t.Fatalf("migrate ate 112: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID, eventID, otherEventID, productA, productB string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Fila','fila-113') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,'active','Semana','multi') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed evento: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type) VALUES ($1,'active','Outra','multi') RETURNING id::text`, storeID,
	).Scan(&otherEventID); err != nil {
		t.Fatalf("seed outro evento: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Vestido','VST1','manual',1000,0,true) RETURNING id::text`, storeID,
	).Scan(&productA); err != nil {
		t.Fatalf("seed produto A: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Blusa','BLU1','manual',1000,0,true) RETURNING id::text`, storeID,
	).Scan(&productB); err != nil {
		t.Fatalf("seed produto B: %v", err)
	}

	// Duplicata viva: o mesmo comprador entrou duas vezes na fila do produto A.
	var keptID, droppedID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,1,'waiting') RETURNING id::text`, eventID, productA,
	).Scan(&keptID); err != nil {
		t.Fatalf("seed fila 1: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,7,'notified') RETURNING id::text`, eventID, productA,
	).Scan(&droppedID); err != nil {
		t.Fatalf("seed fila 2 (duplicata): %v", err)
	}
	// Entrada MORTA do mesmo comprador/produto: não pode ser tocada e não pode
	// impedir a entrada viva.
	if _, err := pool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,99,'expired')`, eventID, productA,
	); err != nil {
		t.Fatalf("seed fila expirada: %v", err)
	}

	// --- aplica a 000113 ---------------------------------------------------
	if err := m.Migrate(113); err != nil {
		t.Fatalf("migrate ate 113: %v", err)
	}

	// 1 e 2: a melhor posição sobreviveu; a outra virou 'cancelled'.
	var keptStatus, droppedStatus string
	var droppedCancelledAt *string
	if err := pool.QueryRow(ctx, `SELECT status FROM waitlist_items WHERE id = $1::uuid`, keptID).Scan(&keptStatus); err != nil {
		t.Fatalf("ler mantida: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status, cancelled_at::text FROM waitlist_items WHERE id = $1::uuid`, droppedID,
	).Scan(&droppedStatus, &droppedCancelledAt); err != nil {
		t.Fatalf("ler colapsada: %v", err)
	}
	if keptStatus != "waiting" {
		t.Errorf("a entrada de melhor posição virou %q — devia ter sido mantida", keptStatus)
	}
	if droppedStatus != "cancelled" {
		t.Errorf("a duplicata virou %q, queria \"cancelled\"", droppedStatus)
	}
	if droppedCancelledAt == nil {
		t.Error("duplicata colapsada ficou sem cancelled_at")
	}

	// 3: o índice barra a segunda entrada viva.
	_, err = pool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,2,'waiting')`, eventID, productA)
	if err == nil {
		t.Fatal("aceitou segunda entrada VIVA do mesmo comprador no mesmo produto/evento")
	}
	if !strings.Contains(err.Error(), "uq_waitlist_live_entry") {
		t.Errorf("erro veio de outra constraint: %v", err)
	}

	// 4: voltar para a fila depois de sair continua permitido — o comprador tem
	// uma 'expired' e uma 'cancelled' e nenhuma delas entra no predicado.
	if _, err := pool.Exec(ctx,
		`UPDATE waitlist_items SET status = 'fulfilled' WHERE id = $1::uuid`, keptID,
	); err != nil {
		t.Fatalf("marcar fulfilled: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,3,'waiting')`, eventID, productA,
	); err != nil {
		t.Fatalf("comprador nao conseguiu voltar para a fila: %v", err)
	}

	// 5: outro produto e outro evento seguem livres.
	if _, err := pool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status)
		 VALUES ($1,$2,'ig-1','@buyer',1,1,'waiting'), ($3,$2,'ig-1','@buyer',1,1,'waiting')`,
		eventID, productB, otherEventID,
	); err != nil {
		t.Fatalf("outro produto/evento foi barrado: %v", err)
	}

	// 6: down.
	if err := m.Migrate(112); err != nil {
		t.Fatalf("down para 112: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'uq_waitlist_live_entry'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar indice apos down: %v", err)
	}
	if n != 0 {
		t.Error("down deixou o indice para tras")
	}
}
