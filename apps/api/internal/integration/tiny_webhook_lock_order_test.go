//go:build integration

package integration

import (
	"context"
	"testing"
	"time"
)

// Production 2026-09-23: recovery held the integration then waited on the
// stock checkpoint; webhook held the checkpoint then waited on integration.
func TestTinyWebhookUsesRecoveryLockOrder(t *testing.T) {
	svc, row, productID, externalID := stockConsistencyFixture(t, syncedProduct)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background()) //nolint:errcheck
	if _, err := holder.Exec(ctx, `SELECT id FROM integrations WHERE id=$1 FOR UPDATE`, row.ID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- svc.enqueueTinyProductWebhook(ctx, row.StoreID, "estoque", externalID) }()
	t.Cleanup(func() {
		_ = holder.Rollback(context.Background())
		cancel()
	})
	// Wait until the webhook actually encounters the integration lock.
	for {
		var waiting bool
		err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid))
			AND query LIKE '%stockWebhookLastPingAt%')`, int32(holder.Conn().PgConn().PID())).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("webhook finished before contention: %v", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// A recovery can now lock this product/checkpoint without forming a cycle.
	if _, err := holder.Exec(ctx, `SELECT id FROM products WHERE id=$1 FOR UPDATE NOWAIT`, productID); err != nil {
		_ = holder.Rollback(context.Background())
		<-finished
		t.Fatalf("webhook locked the product before integration: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var saved bool
	if err := testPool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM erp_stock_sync_state
		WHERE product_id=$1 AND requested_revision=1) AND EXISTS(SELECT 1 FROM integrations
		WHERE id=$2 AND metadata ? 'stockWebhookLastPingAt')`, productID, row.ID).Scan(&saved); err != nil || !saved {
		t.Fatalf("webhook lost durable state after contention: saved=%v err=%v", saved, err)
	}
}
