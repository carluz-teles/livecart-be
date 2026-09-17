//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type tinyStockReaderStub struct {
	providers.ERPProvider
	read func(context.Context, string) (int, error)
}

func (p tinyStockReaderStub) GetProductStock(ctx context.Context, id string) (int, error) {
	return p.read(ctx, id)
}

func TestTinyStockWebhookPersistsBeforeAcknowledgement(t *testing.T) {
	svc, row, _, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { t.Fatal("ingress called ERP"); return nil, nil })
	app := fiber.New()
	handler := NewWebhookHandler(svc, nil, zap.NewNop())
	app.Post("/tiny/:storeId", handler.HandleTiny)
	// Identical payloads must remain valid after another stock transition. There
	// is no provider event identity; product ID is not a delivery deduplication key.
	for range 2 {
		request := httptest.NewRequest("POST", "/tiny/"+row.StoreID, strings.NewReader(`{"tipo":"estoque","dados":{"id":"`+ext+`","saldo":99}}`))
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("status=%d", response.StatusCode)
		}
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE source='tiny' AND payload->>'store_id'=$1`, row.StoreID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("durable deliveries=%d err=%v", count, err)
	}
	var commands []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE source='tiny' AND payload->>'store_id'=$1 LIMIT 1`, row.StoreID).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	var command TinyProductWebhookCommand
	if err := json.Unmarshal(commands, &command); err != nil {
		t.Fatal(err)
	}
	if command.IntegrationID != row.ID || command.ProductID != ext {
		t.Fatalf("wrong owner/payload: %+v", command)
	}
	if strings.Contains(string(commands), "saldo") {
		t.Fatal("persisted raw balance instead of invalidation")
	}
}

func TestTinyStockReadUsesAvailableBalanceAndPreservesFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		available int
		readErr   error
		want      int
	}{
		{name: "missing webhook recovery", available: 0, want: 0},
		{name: "negative available cannot keep selling", available: -2, want: 0},
		{name: "available rather than physical", available: 3, want: 3},
		{name: "API failure is not zero", readErr: errors.New("429"), want: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, id, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { t.Fatal("unexpected detail GET"); return nil, nil })
			reads := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
				return tinyStockReaderStub{read: func(context.Context, string) (int, error) { reads++; return tc.available, tc.readErr }}, nil
			}})
			applied, err := svc.refreshERPAvailableStock(t.Context(), row, ext)
			if (err != nil) != (tc.readErr != nil) || applied != (tc.readErr == nil) || reads != 1 || estoqueDoProduto(t, id) != tc.want {
				t.Fatalf("applied=%v err=%v reads=%d stock=%d", applied, err, reads, estoqueDoProduto(t, id))
			}
		})
	}
}

func TestTinyStockRecoveryClaimsRotateAndDoNotDuplicate(t *testing.T) {
	svc, row, _, _ := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	if _, err := testPool.Exec(t.Context(), `INSERT INTO products(store_id,name,external_source,external_id,keyword,price,stock)
 SELECT $1,'other','tiny','rotation-'||n,(7000+n)::text,100,1 FROM generate_series(1,14) n`, row.StoreID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan []string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { ids, err := svc.claimTinyStockChecks(t.Context(), row.StoreID); results <- ids; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for ids := range results {
		if len(ids) > 10 {
			t.Fatal("unbounded batch")
		}
		for _, id := range ids {
			if seen[id] {
				t.Fatal("replicas claimed same product")
			}
			seen[id] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("replicas exceeded one batch: %d", len(seen))
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata=metadata||jsonb_build_object('stockRecoveryClaimedAt',now()-interval '2 minutes') WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := svc.claimTinyStockChecks(t.Context(), row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if seen[id] {
			t.Fatal("rotation repeated a recent attempt")
		}
		seen[id] = true
	}
	if len(seen) != 15 {
		t.Fatalf("visited=%d want 15", len(seen))
	}
}

func TestTinyQueuedStockDoesNotFollowReplacedIntegration(t *testing.T) {
	svc, row, _, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) {
		t.Fatal("ERP called for former integration")
		return nil, nil
	})
	err := svc.ProcessTinyProductWebhook(t.Context(), TinyProductWebhookCommand{StoreID: row.StoreID, IntegrationID: "replaced", ProductID: ext, Kind: "estoque"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTinyStockReadCannotOverwriteConcurrentAdmission(t *testing.T) {
	svc, row, id, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
			return 10, testRepo.DecrementProductStock(t.Context(), id, 1)
		}}, nil
	}})
	applied, err := svc.refreshERPAvailableStock(t.Context(), row, ext)
	if err == nil || applied || estoqueDoProduto(t, id) != 9 {
		t.Fatalf("stale stock applied=%v err=%v", applied, err)
	}
}

func TestTinyStockHealthUsesDeliveryTimestamp(t *testing.T) {
	svc, row, _, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET created_at=now()-interval '2 days' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueTinyProductWebhook(t.Context(), row.StoreID, "estoque", ext); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.repo.ListTinyIntegrationsWithStaleStockWebhook(t.Context(), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range stale {
		if i.IntegrationID == row.ID {
			t.Fatal("durable delivery reported as absent")
		}
	}
}

func TestTinyWebhookPingPreservesStockCheckpoints(t *testing.T) {
	svc, row, _, _ := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET metadata='{"stockWebhookLastPingAt":"2026-09-17T12:00:00Z","stockRecoveryClaimedAt":"2026-09-17T12:00:00Z","custom_setting":true}' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	svc.RecordWebhookPing(t.Context(), row.StoreID, "tiny")
	var kept bool
	if err := testPool.QueryRow(t.Context(), `SELECT metadata ? 'webhookLastPingAt' AND metadata ? 'stockWebhookLastPingAt' AND metadata ? 'stockRecoveryClaimedAt' AND metadata->>'custom_setting'='true' FROM integrations WHERE id=$1`, row.ID).Scan(&kept); err != nil || !kept {
		t.Fatalf("lost metadata: %v %v", kept, err)
	}
}

func TestTinyStockWebhookDoesNotAcknowledgeFailedPersistence(t *testing.T) {
	svc, row, _, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { t.Fatal("ingress reached ERP"); return nil, nil })
	closed, err := pgxpool.NewWithConfig(t.Context(), testPool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	repoCopy := *svc.repo
	repoCopy.pool = closed
	svc.repo = &repoCopy
	app := fiber.New()
	handler := NewWebhookHandler(svc, nil, zap.NewNop())
	app.Post("/tiny/:storeId", handler.HandleTiny)
	request := httptest.NewRequest("POST", "/tiny/"+row.StoreID, strings.NewReader(`{"tipo":"estoque","dados":{"id":"`+ext+`"}}`))
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatalf("failed persistence returned %d", response.StatusCode)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE source='tiny' AND payload->>'store_id'=$1`, row.StoreID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected persisted event: %d %v", count, err)
	}
}
