//go:build integration

package migrationtest

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration154RemovesOnlyIntegrationLogs(t *testing.T) {
	dbURL, cleanup := freshDB(t, mustEnv(t))
	defer cleanup()
	m, err := migrate.New(migrationsURL(t), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Migrate(153); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var integrationID string
	err = pool.QueryRow(ctx, `
		WITH store AS (
			INSERT INTO stores (name, slug) VALUES ('Log removal test', 'log-removal') RETURNING id
		)
		INSERT INTO integrations (store_id, type, provider, status)
		SELECT id, 'erp', 'tiny', 'active' FROM store RETURNING id::text
	`).Scan(&integrationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integration_logs (integration_id, status, response_payload)
		VALUES ($1::uuid, 'success', '{"legacy":"payload"}')
	`, integrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_events (integration_id, provider, event_type, payload)
		VALUES ($1::uuid, 'tiny', 'test', '{}')
	`, integrationID); err != nil {
		t.Fatal(err)
	}

	var tablesBefore int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tablesBefore)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(154); err != nil {
		t.Fatal(err)
	}
	version, dirty, err := m.Version()
	if err != nil || version != 154 || dirty {
		t.Fatalf("migration not clean: version=%d dirty=%v err=%v", version, dirty, err)
	}
	var absent bool
	err = pool.QueryRow(ctx, `SELECT to_regclass('public.integration_logs') IS NULL`).Scan(&absent)
	if err != nil || !absent {
		t.Fatalf("integration_logs still exists: absent=%v err=%v", absent, err)
	}
	var tablesAfter int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tablesAfter)
	if err != nil {
		t.Fatal(err)
	}
	if tablesAfter != tablesBefore-1 {
		t.Fatalf("unexpected table removal: before=%d after=%d", tablesBefore, tablesAfter)
	}
	var status string
	err = pool.QueryRow(ctx, `SELECT status FROM integrations WHERE id=$1::uuid`, integrationID).Scan(&status)
	if err != nil || status != "active" {
		t.Fatalf("integration changed: status=%q err=%v", status, err)
	}
	var webhooks int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE integration_id=$1::uuid`, integrationID,
	).Scan(&webhooks)
	if err != nil || webhooks != 1 {
		t.Fatalf("webhook queue changed: count=%d err=%v", webhooks, err)
	}

	// A rollback restores compatibility with the previous binary, not the data.
	if err := m.Migrate(153); err != nil {
		t.Fatal(err)
	}
	var restoredLogs int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM integration_logs`).Scan(&restoredLogs)
	if err != nil || restoredLogs != 0 {
		t.Fatalf("rollback must restore an empty table: count=%d err=%v", restoredLogs, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO integration_logs (integration_id, entity_type, entity_id, direction, status,
			request_payload, response_payload, error_message)
		VALUES ($1::uuid, 'order', gen_random_uuid(), 'outbound', 'error', '{}', '{}', 'test')
	`, integrationID); err != nil {
		t.Fatalf("legacy insert after rollback: %v", err)
	}
	if err := m.Migrate(154); err != nil {
		t.Fatalf("reapply removal: %v", err)
	}
}
