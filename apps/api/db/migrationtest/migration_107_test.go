package migrationtest

// Teste da migration 000107: a unique TOTAL de carts vira PARCIAL.
//
// O que precisa ficar provado:
//   1. antes (000106) dois carrinhos por (evento, comprador) são impossíveis —
//      é a razão de o reopen ser destrutivo hoje;
//   2. depois (000107) pagar/expirar/cancelar liberam a vaga;
//   3. mas dois carrinhos ABERTOS continuam impossíveis;
//   4. payment_status NULL não escapa do índice (a armadilha do NOT IN).

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

type cartFixture struct {
	pool    *pgxpool.Pool
	eventID string
	buyer   string
	seq     int
}

// insertCart tenta criar um carrinho e devolve o erro cru, para o teste decidir.
func (f *cartFixture) insertCart(status string, paymentStatus *string) error {
	f.seq++
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1, $2, $2, $3, $4, $5, $6)`,
		f.eventID, f.buyer,
		fmt.Sprintf("tok-%s-%d", f.buyer, f.seq),
		f.seq,
		status, paymentStatus,
	)
	return err
}

func strptr(s string) *string { return &s }

func TestMigration000107PartialUnique(t *testing.T) {
	adminURL := mustEnv(t)
	url, cleanup := freshDB(t, adminURL)
	defer cleanup()

	m, err := migrate.New(migrationsURL(t), url)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	// --- estado ANTES da 000107 -------------------------------------------
	if err := m.Migrate(106); err != nil {
		t.Fatalf("migrate ate 104: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	ctx := context.Background()

	var storeID, eventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Unique Test','uniq-105') RETURNING id::text`,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'active','Unique 105') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	f := &cartFixture{pool: pool, eventID: eventID, buyer: "buyer_105"}

	if err := f.insertCart("active", strptr("pending")); err != nil {
		t.Fatalf("primeiro carrinho devia entrar: %v", err)
	}
	// Marca como pago — com a unique TOTAL, isso não libera a vaga.
	if _, err := pool.Exec(ctx,
		`UPDATE carts SET payment_status = 'paid', paid_at = now() WHERE event_id = $1`, eventID,
	); err != nil {
		t.Fatalf("marcar pago: %v", err)
	}
	if err := f.insertCart("active", strptr("pending")); err == nil {
		t.Error("antes da 000107, um 2o carrinho devia ser IMPOSSIVEL mesmo com o 1o pago — e o motivo do reopen destrutivo")
	}

	// --- aplica a 000107 ---------------------------------------------------
	if err := m.Migrate(107); err != nil {
		t.Fatalf("migrate ate 105: %v", err)
	}

	// 2. Pago liberou a vaga.
	if err := f.insertCart("active", strptr("pending")); err != nil {
		t.Fatalf("depois da 000107 o carrinho pago devia liberar a vaga: %v", err)
	}

	// 3. Dois ABERTOS continuam impossíveis.
	if err := f.insertCart("active", strptr("pending")); err == nil {
		t.Error("dois carrinhos ABERTOS passaram — a unificacao por evento quebrou")
	}

	// 4. payment_status NULL tem de ocupar a vaga (armadilha do NOT IN).
	//    Zera tudo e recomeça com NULL explícito.
	if _, err := pool.Exec(ctx, `DELETE FROM carts WHERE event_id = $1`, eventID); err != nil {
		t.Fatalf("limpar carrinhos: %v", err)
	}
	if err := f.insertCart("active", nil); err != nil {
		t.Fatalf("carrinho com payment_status NULL devia entrar: %v", err)
	}
	if err := f.insertCart("active", nil); err == nil {
		t.Error("dois carrinhos com payment_status NULL passaram — o predicado esta usando NOT IN sem tratar NULL")
	}

	// 5. Cada estado terminal libera a vaga, um a um.
	for _, tc := range []struct {
		name   string
		update string
	}{
		{"expirado", `UPDATE carts SET status='expired' WHERE event_id=$1`},
		{"cancelado", `UPDATE carts SET status='cancelled' WHERE event_id=$1`},
		{"estornado", `UPDATE carts SET payment_status='refunded' WHERE event_id=$1`},
	} {
		t.Run(tc.name+" libera a vaga", func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM carts WHERE event_id = $1`, eventID); err != nil {
				t.Fatalf("limpar: %v", err)
			}
			if err := f.insertCart("active", strptr("pending")); err != nil {
				t.Fatalf("carrinho base: %v", err)
			}
			if _, err := pool.Exec(ctx, tc.update, eventID); err != nil {
				t.Fatalf("aplicar estado: %v", err)
			}
			if err := f.insertCart("active", strptr("pending")); err != nil {
				t.Errorf("estado %q devia liberar a vaga: %v", tc.name, err)
			}
		})
	}

	// --- down volta para a constraint total --------------------------------
	if _, err := pool.Exec(ctx, `DELETE FROM carts WHERE event_id = $1`, eventID); err != nil {
		t.Fatalf("limpar antes do down: %v", err)
	}
	if err := f.insertCart("active", strptr("pending")); err != nil {
		t.Fatalf("carrinho unico antes do down: %v", err)
	}
	if err := m.Migrate(106); err != nil {
		t.Fatalf("down para 104: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'carts_one_open_per_event_buyer'`,
	).Scan(&n); err != nil {
		t.Fatalf("checar indice apos down: %v", err)
	}
	if n != 0 {
		t.Error("down deixou o indice parcial para tras")
	}
}
