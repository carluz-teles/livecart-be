//go:build integration

package migrationtest

import (
	"bytes"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration170ProtectsOnlyOpenVIPDeadlines(t *testing.T) {
	dbURL, cleanup := freshDB(t, mustEnv(t))
	defer cleanup()
	m, err := migrate.New(migrationsURL(t), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(169); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(t.Context(), `
        WITH s AS (INSERT INTO stores(name,slug) VALUES('VIP migration','vip-migration') RETURNING id)
        INSERT INTO live_events(store_id,status,title,ends_at) SELECT id,'active','VIP migration',now()+interval '1 day' FROM s;
        INSERT INTO vip_handles(store_id,platform_handle,removed_at)
        SELECT id,'vip',NULL FROM stores UNION ALL SELECT id,'removed',now() FROM stores;
        INSERT INTO carts(event_id,store_id,platform_user_id,platform_handle,token,short_id,
                          status,payment_status,expires_at,external_order_id)
        SELECT e.id,e.store_id,f.label,f.handle,f.label,f.number,f.status,f.payment,
               now()+interval '1 hour','ERP-'||f.label
        FROM live_events e CROSS JOIN (VALUES
          ('open-a','vip',1,'checkout','pending'),
          ('open-b','vip',2,'active','pending'),
          ('paid','vip',3,'checkout','paid'),
          ('expired','vip',4,'expired','pending'),
          ('normal','ordinary',5,'checkout','pending'),
          ('removed','removed',6,'checkout','pending')
        ) AS f(label,handle,number,status,payment);`)
	if err != nil {
		t.Fatal(err)
	}
	readOtherFields := func() []byte {
		t.Helper()
		var data []byte
		if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(c)-'expires_at' ORDER BY id) FROM carts c`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := readOtherFields()
	for _, version := range []uint{170, 169, 170} {
		if err := m.Migrate(version); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, readOtherFields()) {
			t.Fatal("migration changed cart state, ERP binding or other fields besides the deadline")
		}
		var protected, preserved int
		if err := pool.QueryRow(t.Context(), `SELECT
            count(*) FILTER (WHERE platform_user_id IN ('open-a','open-b') AND expires_at IS NULL),
            count(*) FILTER (WHERE platform_user_id NOT IN ('open-a','open-b') AND expires_at IS NOT NULL)
            FROM carts`).Scan(&protected, &preserved); err != nil {
			t.Fatal(err)
		}
		if protected != 2 || preserved != 4 {
			t.Fatalf("version=%d protected=%d preserved=%d; expected 2/4", version, protected, preserved)
		}
	}
}
