//go:build integration

package integration

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestWebhookPingSkipsLockedIntegration(t *testing.T) {
	svc, row, _, _ := stockConsistencyFixture(t, syncedProduct)
	core, logs := observer.New(zap.WarnLevel)
	svc.logger = zap.New(core)
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context()) //nolint:errcheck
	if _, err := tx.Exec(t.Context(), `SELECT id FROM integrations WHERE id=$1 FOR UPDATE`, row.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = tx.Rollback(t.Context())
		<-done
		t.Fatal("health ping waited for the durable transaction")
	}
	if logs.Len() != 0 {
		t.Fatalf("skipping contention must not warn: %v", logs.All())
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny")
	var recorded bool
	if err := testPool.QueryRow(t.Context(), `SELECT metadata ? 'webhookLastPingAt' FROM integrations WHERE id=$1`, row.ID).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("next delivery did not record health: %v %v", recorded, err)
	}
}

func TestWebhookPingCoalescesConcurrentDeliveries(t *testing.T) {
	svc, row, _, _ := stockConsistencyFixture(t, syncedProduct)
	_, other, _, _ := stockConsistencyFixture(t, syncedProduct)
	svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny")
	version := func() string {
		t.Helper()
		var result string
		if err := testPool.QueryRow(t.Context(), `SELECT xmin::text FROM integrations WHERE id=$1`, row.ID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := version()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny") }()
	}
	wg.Wait()
	if version() != before {
		t.Fatal("fresh health was written again during a webhook burst")
	}
	var untouched bool
	if err := testPool.QueryRow(t.Context(), `SELECT NOT (COALESCE(metadata,'{}') ? 'webhookLastPingAt') FROM integrations WHERE id=$1`, other.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("another store was updated: %v %v", untouched, err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata=COALESCE(metadata,'{}')||jsonb_build_object('webhookLastPingAt',now()-interval '2 minutes') WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	svc.RecordWebhookPing(t.Context(), row.StoreID, "instagram")
	var stale bool
	if err := testPool.QueryRow(t.Context(), `SELECT (metadata->>'webhookLastPingAt')::timestamptz<now()-interval '1 minute' FROM integrations WHERE id=$1`, row.ID).Scan(&stale); err != nil || !stale {
		t.Fatalf("another provider was updated: %v %v", stale, err)
	}
	svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny")
	if err := testPool.QueryRow(t.Context(), `SELECT (metadata->>'webhookLastPingAt')::timestamptz>now()-interval '1 minute' FROM integrations WHERE id=$1`, row.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("stale health was not refreshed: %v %v", untouched, err)
	}
}
