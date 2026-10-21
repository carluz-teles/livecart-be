//go:build integration

package migrationtest

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/internal/integration/providers"
)

func migration180DB(t *testing.T) (*migrate.Migrate, *pgxpool.Pool) {
	t.Helper()
	dbURL, cleanup := freshDB(t, mustEnv(t))
	t.Cleanup(cleanup)
	m, err := migrate.New(migrationsURL(t), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if err := m.Migrate(179); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return m, pool
}

type migration180Buyer struct {
	oldCart string
	event   string
	index   string
}

// VIP uses different events and user IDs so only the store/handle index can reject it.
// Regular carts use the same event and only the event/user index applies.
func seedMigration180Buyer(t *testing.T, pool *pgxpool.Pool, vip bool) migration180Buyer {
	t.Helper()
	var store, event string
	err := pool.QueryRow(t.Context(), `INSERT INTO stores(name,slug)
		VALUES('Missing ERP order',gen_random_uuid()::text) RETURNING id::text`).Scan(&store)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,status,title,ends_at)
		VALUES($1,'active','Missing ERP order',now()+interval '1 day') RETURNING id::text`, store).Scan(&event)
	if err != nil {
		t.Fatal(err)
	}
	buyer := migration180Buyer{event: event, index: "carts_one_open_per_event_buyer"}
	err = pool.QueryRow(t.Context(), `INSERT INTO carts
		(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,
		 payment_status,never_expires,external_order_id,erp_order_status,erp_order_state)
		VALUES($1,$2,'buyer','buyer',gen_random_uuid()::text,1,'checkout',
		 'pending',$3,'old-erp-order','nao_encontrado','open') RETURNING id::text`,
		event, store, vip).Scan(&buyer.oldCart)
	if err != nil {
		t.Fatal(err)
	}
	if vip {
		buyer.index = "carts_one_eternal_per_store_buyer"
		err = pool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,status,title,ends_at)
			VALUES($1,'active','Next VIP event',now()+interval '1 day') RETURNING id::text`, store).Scan(&buyer.event)
		if err != nil {
			t.Fatal(err)
		}
	}
	return buyer
}

func (b migration180Buyer) insertCart(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	_, err := pool.Exec(t.Context(), `INSERT INTO carts
		(event_id,store_id,platform_user_id,platform_handle,token,short_id,status,
		 payment_status,never_expires,external_order_id,erp_order_status)
		SELECT $2,store_id,CASE WHEN never_expires THEN gen_random_uuid()::text ELSE platform_user_id END,
		 platform_handle,gen_random_uuid()::text,
		 (SELECT MAX(short_id)+1 FROM carts),'active','pending',never_expires,'new-erp-order','aberto'
		FROM carts WHERE id=$1`, b.oldCart, b.event)
	return err
}

func migration180CartSnapshot(t *testing.T, pool *pgxpool.Pool) []byte {
	t.Helper()
	var data []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM carts c`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func migration180Indexes(t *testing.T, pool *pgxpool.Pool) map[string]uint32 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT c.relname,c.relfilenode,i.indisvalid,i.indisready,i.indisunique
		FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
		WHERE c.relname IN ('carts_one_open_per_event_buyer','carts_one_eternal_per_store_buyer')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexes := map[string]uint32{}
	for rows.Next() {
		var name string
		var file uint32
		var valid, ready, unique bool
		if err := rows.Scan(&name, &file, &valid, &ready, &unique); err != nil {
			t.Fatal(err)
		}
		if !valid || !ready || !unique {
			t.Fatalf("invalid uniqueness guard %s: valid=%v ready=%v unique=%v", name, valid, ready, unique)
		}
		indexes[name] = file
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 2 {
		t.Fatalf("expected both purchase indexes, got %v", indexes)
	}
	return indexes
}

func assertMigration180Conflict(t *testing.T, err error, index string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != index {
		t.Fatalf("expected uniqueness violation of %s, got %v", index, err)
	}
}

func TestMigration180ForwardBackwardPurchaseConstraints(t *testing.T) {
	m, pool := migration180DB(t)
	buyers := []migration180Buyer{
		seedMigration180Buyer(t, pool, false),
		seedMigration180Buyer(t, pool, true),
	}
	for _, buyer := range buyers {
		assertMigration180Conflict(t, buyer.insertCart(t, pool), buyer.index)
	}
	for _, version := range []uint{180, 179, 180} {
		before := migration180CartSnapshot(t, pool)
		indexes := migration180Indexes(t, pool)
		if err := m.Migrate(version); err != nil {
			t.Fatal(err)
		}
		got, dirty, err := m.Version()
		if err != nil || dirty || got != version {
			t.Fatalf("version=%d dirty=%v err=%v", got, dirty, err)
		}
		if !bytes.Equal(before, migration180CartSnapshot(t, pool)) {
			t.Fatal("migration changed cart history, financial state or ERP links")
		}
		for name, file := range migration180Indexes(t, pool) {
			if indexes[name] == file {
				t.Fatalf("migration %d did not rebuild %s", version, name)
			}
		}
		for _, buyer := range buyers {
			if version == 179 {
				assertMigration180Conflict(t, buyer.insertCart(t, pool), buyer.index)
				continue
			}
			if err := buyer.insertCart(t, pool); err != nil {
				t.Fatalf("old missing order must allow a new purchase: %v", err)
			}
			assertMigration180Conflict(t, buyer.insertCart(t, pool), buyer.index)
			_, err := pool.Exec(t.Context(), `UPDATE carts SET erp_order_status='aberto' WHERE id=$1`, buyer.oldCart)
			assertMigration180Conflict(t, err, buyer.index)
		}
		// Finish the newly seeded purchases so a lossless downgrade is possible.
		if _, err := pool.Exec(t.Context(), `UPDATE carts SET status='cancelled'
			WHERE external_order_id='new-erp-order'`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigration180RejectsConflictingDowngradeAtomically(t *testing.T) {
	for _, vip := range []bool{false, true} {
		name := "event"
		if vip {
			name = "vip"
		}
		t.Run(name, func(t *testing.T) {
			m, pool := migration180DB(t)
			buyer := seedMigration180Buyer(t, pool, vip)
			if err := m.Migrate(180); err != nil {
				t.Fatal(err)
			}
			if err := buyer.insertCart(t, pool); err != nil {
				t.Fatal(err)
			}
			before := migration180CartSnapshot(t, pool)
			indexes := migration180Indexes(t, pool)
			err := m.Migrate(179)
			if err == nil || !strings.Contains(err.Error(), buyer.index) {
				t.Fatalf("downgrade must reject conflicting %s history: %v", name, err)
			}
			// The failed explicit transaction is rolled back when its connection
			// closes; migrate's dirty marker still requires operator attention.
			m.Close()
			if !bytes.Equal(before, migration180CartSnapshot(t, pool)) {
				t.Fatal("failed downgrade changed cart data")
			}
			var accepts bool
			if err := pool.QueryRow(t.Context(), `SELECT erp_order_accepts_items('nao_encontrado')`).Scan(&accepts); err != nil {
				t.Fatal(err)
			}
			if accepts {
				t.Fatal("failed downgrade left the old function installed")
			}
			for name, file := range migration180Indexes(t, pool) {
				if indexes[name] != file {
					t.Fatalf("failed downgrade left %s partially rebuilt", name)
				}
			}
			assertMigration180Conflict(t, buyer.insertCart(t, pool), buyer.index)
		})
	}
}

func TestMigration180SQLAndGoPurchaseBoundaryAgree(t *testing.T) {
	m, pool := migration180DB(t)
	if err := m.Migrate(180); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		status  string
		accepts bool
	}{
		{"", true}, {"aberto", true}, {"aprovado", true}, {"dados_incompletos", true},
		{"unknown", true}, {"preparando_envio", false}, {"faturado", false},
		{"pronto_envio", false}, {"enviado", false}, {"entregue", false},
		{"nao_entregue", false}, {"cancelado", false}, {"nao_encontrado", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			var accepts bool
			if err := pool.QueryRow(t.Context(), `SELECT erp_order_accepts_items($1)`, tc.status).Scan(&accepts); err != nil {
				t.Fatal(err)
			}
			if accepts != tc.accepts || !providers.ERPOrderStatus(tc.status).FechadoParaNovosItens() != tc.accepts {
				t.Fatalf("status %q: SQL/Go must both accept=%v (SQL=%v)", tc.status, tc.accepts, accepts)
			}
		})
	}
	var acceptsNull bool
	if err := pool.QueryRow(t.Context(), `SELECT erp_order_accepts_items(NULL)`).Scan(&acceptsNull); err != nil {
		t.Fatal(err)
	}
	if !acceptsNull {
		t.Fatal("a cart with no ERP observation must still accept items")
	}
}
