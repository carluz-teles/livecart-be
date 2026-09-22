//go:build integration

package migrationtest

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration169PreservesExistingStockCheckpoint(t *testing.T) {
	dbURL, cleanup := freshDB(t, mustEnv(t))
	defer cleanup()
	m, err := migrate.New(migrationsURL(t), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(168); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var product string
	err = pool.QueryRow(t.Context(), `WITH store AS (
		INSERT INTO stores(name,slug) VALUES('Checkpoint migration','checkpoint-migration') RETURNING id
	) INSERT INTO products(store_id,name,keyword,price,stock,external_source,external_id)
	SELECT id,'Existing product','T169',1000,7,'tiny','123' FROM store RETURNING id::text`).Scan(&product)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state
		(product_id,last_attempt_at,last_success_at,deferred_at)
		VALUES($1,'2026-09-01 12:00:00+00','2026-09-01 11:00:00+00','2026-09-01 12:00:00+00')`, product); err != nil {
		t.Fatal(err)
	}
	checkExisting := func() {
		t.Helper()
		var preserved bool
		err := pool.QueryRow(t.Context(), `SELECT p.stock=7 AND p.price=1000
			AND s.last_attempt_at='2026-09-01 12:00:00+00'::timestamptz
			AND s.last_success_at='2026-09-01 11:00:00+00'::timestamptz
			AND s.deferred_at='2026-09-01 12:00:00+00'::timestamptz
			FROM products p JOIN erp_stock_sync_state s ON s.product_id=p.id WHERE p.id=$1`, product).Scan(&preserved)
		if err != nil || !preserved {
			t.Fatalf("existing inventory/checkpoint changed: preserved=%v err=%v", preserved, err)
		}
	}
	for _, version := range []uint{169, 168, 169} {
		if err := m.Migrate(version); err != nil {
			t.Fatal(err)
		}
		got, dirty, err := m.Version()
		if err != nil || dirty || got != version {
			t.Fatalf("migration state: version=%d dirty=%v err=%v", got, dirty, err)
		}
		checkExisting()
		if version == 169 {
			var ready bool
			err := pool.QueryRow(t.Context(), `SELECT requested_revision=0 AND completed_revision=0
				AND read_owner IS NULL AND read_until IS NULL FROM erp_stock_sync_state WHERE product_id=$1`, product).Scan(&ready)
			if err != nil || !ready {
				t.Fatalf("existing checkpoint received invalid defaults: ready=%v err=%v", ready, err)
			}
		}
	}
}
