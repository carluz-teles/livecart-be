//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/database"
)

type resyncTestProvider struct {
	providers.ERPProvider
	read func(string) (*providers.ERPProduct, error)
}

func (p resyncTestProvider) GetProduct(_ context.Context, id string) (*providers.ERPProduct, error) {
	return p.read(id)
}

func resyncFixture(t *testing.T, count int, read func(string) (*providers.ERPProduct, error)) (*Service, *IntegrationRow) {
	t.Helper()
	svc, row, _, _ := stockConsistencyFixture(t, read)
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(),
		TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
			return resyncTestProvider{read: read}, nil
		}})
	for i := 1; i < count; i++ {
		_, err := testPool.Exec(t.Context(), `INSERT INTO products
			(store_id,name,external_source,external_id,keyword,price,stock)
			VALUES($1,'Catalog test','tiny',$2,$3,1000,10)`, row.StoreID, fmt.Sprintf("resync-%d", i), fmt.Sprintf("%04d", 9000+i))
		if err != nil {
			t.Fatal(err)
		}
	}
	return svc, row
}

func resyncCommand(t *testing.T, row *IntegrationRow) ERPResyncCommand {
	t.Helper()
	command := ERPResyncCommand{IntegrationID: row.ID, StoreID: row.StoreID}
	if err := testPool.QueryRow(t.Context(), `SELECT run_id::text,dispatch_id::text
		FROM erp_resync_jobs WHERE integration_id=$1`, row.ID).Scan(&command.RunID, &command.DispatchID); err != nil {
		t.Fatal(err)
	}
	return command
}

func resyncProgressForTest(t *testing.T, svc *Service, row *IntegrationRow) *ERPResyncProgress {
	t.Helper()
	progress, err := svc.repo.resyncProgressForStore(t.Context(), row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	return progress[row.ID]
}

func syncedProduct(id string) (*providers.ERPProduct, error) {
	return &providers.ERPProduct{ID: id, Name: "Updated product", Price: 1000,
		Stock: 5, StockKnown: true, Active: true}, nil
}

func TestERPResyncResumesAndPreservesFinalProgress(t *testing.T) {
	calls := map[string]int{}
	svc, row := resyncFixture(t, 7, func(id string) (*providers.ERPProduct, error) {
		calls[id]++
		if id == "resync-3" {
			return nil, errors.New("product removed in ERP")
		}
		return syncedProduct(id)
	})
	// Exercise the same pgx protocol used in production (not only test defaults).
	pool, err := database.NewPool(t.Context(), testPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc.repo = NewRepository(sqlc.New(pool), pool)
	_, err = testPool.Exec(t.Context(), `UPDATE integrations SET metadata='{"warehouse":"preserve"}' WHERE id=$1`, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	start, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID})
	if err != nil || start.Total != 7 || !start.Running() {
		t.Fatalf("start: %+v, %v", start, err)
	}
	first := resyncCommand(t, row)
	if err := svc.RunERPResync(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if p := resyncProgressForTest(t, svc, row); p.Done != 5 || !p.Running() {
		t.Fatalf("first batch: %+v", p)
	}
	// Duplicate delivery after checkpointing must not process the next batch.
	if err := svc.RunERPResync(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if p := resyncProgressForTest(t, svc, row); p.Done != 5 {
		t.Fatalf("stale delivery advanced: %+v", p)
	}
	// A newly constructed service resumes from the durable cursor after a deploy.
	restarted := reconnectTestService(t)
	restarted.factory, restarted.productSyncer = svc.factory, svc.productSyncer
	if err := restarted.RunERPResync(t.Context(), resyncCommand(t, row)); err != nil {
		t.Fatal(err)
	}
	p := resyncProgressForTest(t, restarted, row)
	if p.Done != 7 || p.Succeeded != 6 || p.Failed != 1 || p.Status != "completed_with_errors" || p.FinishedAt == nil {
		t.Fatalf("final counters: %+v", p)
	}
	for id, n := range calls {
		if n != 1 {
			t.Errorf("product %s read %d times", id, n)
		}
	}
	var snapshotSize int
	var metadata string
	if err := testPool.QueryRow(t.Context(), `SELECT cardinality(j.product_ids),i.metadata->>'warehouse'
		FROM erp_resync_jobs j JOIN integrations i ON i.id=j.integration_id WHERE i.id=$1`, row.ID).
		Scan(&snapshotSize, &metadata); err != nil {
		t.Fatal(err)
	}
	if snapshotSize != 0 || metadata != "preserve" {
		t.Fatalf("snapshot=%d metadata=%q", snapshotSize, metadata)
	}
	// A new run must never reuse the archived/completed task identity.
	newRun, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID})
	if err != nil || newRun.RunID == start.RunID || newRun.Done != 0 {
		t.Fatalf("new run: %+v, %v", newRun, err)
	}
}

func TestERPResyncConcurrentStartsShareOneRun(t *testing.T) {
	svc, row := resyncFixture(t, 1, syncedProduct)
	var wg sync.WaitGroup
	results := make(chan *ERPResyncProgress, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			p, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID})
			results <- p
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run := ""
	for p := range results {
		if run != "" && p.RunID != run {
			t.Fatal("concurrent clicks created different runs")
		}
		run = p.RunID
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox
		WHERE payload->>'run_id'=$1`, run).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queued commands=%d, want 1", count)
	}
}

func TestERPResyncRecoversExpiredLeaseAndFencesOldWorker(t *testing.T) {
	svc, row := resyncFixture(t, 1, syncedProduct)
	if _, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID}); err != nil {
		t.Fatal(err)
	}
	oldCommand := resyncCommand(t, row)
	oldJob, err := svc.repo.claimERPResync(t.Context(), oldCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_resync_jobs SET lease_until=now()-interval '1 second'
		WHERE integration_id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	svc.RecoverERPResync(t.Context())
	newCommand := resyncCommand(t, row)
	if newCommand.DispatchID == oldCommand.DispatchID || newCommand.RunID != oldCommand.RunID {
		t.Fatalf("recovery lost run or reused dispatch: %+v", newCommand)
	}
	if err := svc.repo.advanceERPResync(t.Context(), oldJob, nil); !errors.Is(err, errERPResyncLeaseLost) {
		t.Fatalf("stale worker checkpoint: %v", err)
	}
	if _, err := svc.repo.releaseERPResync(t.Context(), oldJob, nil); !errors.Is(err, errERPResyncLeaseLost) {
		t.Fatalf("stale worker release: %v", err)
	}
	if err := svc.RunERPResync(t.Context(), newCommand); err != nil {
		t.Fatal(err)
	}
	if p := resyncProgressForTest(t, svc, row); p.Status != "completed" || p.Succeeded != 1 {
		t.Fatalf("recovery: %+v", p)
	}
}

func TestERPResyncTemporaryFailureDoesNotSkipProduct(t *testing.T) {
	fail := true
	svc, row := resyncFixture(t, 1, func(id string) (*providers.ERPProduct, error) {
		if fail {
			return nil, context.DeadlineExceeded
		}
		return syncedProduct(id)
	})
	if _, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RunERPResync(t.Context(), resyncCommand(t, row)); err != nil {
		t.Fatal(err)
	}
	p := resyncProgressForTest(t, svc, row)
	if p.Status != "retrying" || p.Done != 0 || p.Failed != 0 {
		t.Fatalf("temporary failure skipped product: %+v", p)
	}
	fail = false
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_resync_jobs SET next_attempt_at=now()
		WHERE integration_id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	svc.RecoverERPResync(t.Context())
	if err := svc.RunERPResync(t.Context(), resyncCommand(t, row)); err != nil {
		t.Fatal(err)
	}
	if p := resyncProgressForTest(t, svc, row); p.Succeeded != 1 || p.Failed != 0 || p.Status != "completed" {
		t.Fatalf("retry did not finish: %+v", p)
	}
}

func TestERPResyncQueueRecoveryPreservesOriginalDelivery(t *testing.T) {
	svc, row := resyncFixture(t, 1, syncedProduct)
	if _, err := svc.StartERPResync(t.Context(), StartERPResyncInput{StoreID: row.StoreID, IntegrationID: row.ID}); err != nil {
		t.Fatal(err)
	}
	original := resyncCommand(t, row)
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_resync_jobs SET updated_at=now()-interval '6 minutes'
		WHERE integration_id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	svc.RecoverERPResync(t.Context())
	if resyncCommand(t, row).DispatchID != original.DispatchID {
		t.Fatal("recovery invalidated a command still waiting in the queue")
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox
		WHERE payload->>'run_id'=$1`, original.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("recovery did not create a fresh task for lost/archived delivery: %d", count)
	}
	if err := svc.RunERPResync(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if p := resyncProgressForTest(t, svc, row); p.Status != "completed" {
		t.Fatalf("original queue delivery did not progress: %+v", p)
	}
}
