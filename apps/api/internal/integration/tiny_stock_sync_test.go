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
	"livecart/apps/api/internal/inventory"
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

func TestTinyStockInvalidationsCoalesceWithoutLosingNextChange(t *testing.T) {
	svc, row, id, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { t.Fatal("unexpected details read"); return nil, nil })
	enqueue := func() {
		t.Helper()
		if err := svc.enqueueTinyProductWebhook(t.Context(), row.StoreID, "estoque", ext); err != nil {
			t.Fatal(err)
		}
	}
	enqueue()
	enqueue()
	reads := 0
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
			reads++
			if reads == 1 {
				return 3, nil
			}
			if reads == 2 {
				enqueue()
				return 4, nil
			} // Superseded while GET is in flight.
			return 7, nil
		}}, nil
	}})
	check := func(revision int64) {
		t.Helper()
		ok, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, revision)
		if err != nil || !ok {
			t.Fatalf("revision %d: applied=%v err=%v", revision, ok, err)
		}
	}
	check(1)
	check(2)
	if reads != 1 || estoqueDoProduto(t, id) != 3 {
		t.Fatalf("overlapping invalidations made %d reads", reads)
	}
	enqueue()
	if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 3); err == nil || applied {
		t.Fatalf("superseded GET was accepted: applied=%v err=%v", applied, err)
	}
	if estoqueDoProduto(t, id) != 3 {
		t.Fatal("superseded GET overwrote cached stock")
	}
	var requested, completed int64
	if err := testPool.QueryRow(t.Context(), `SELECT requested_revision,completed_revision FROM erp_stock_sync_state WHERE product_id=$1`, id).Scan(&requested, &completed); err != nil {
		t.Fatal(err)
	}
	if requested != 4 || completed != 2 {
		t.Fatalf("lost dirty revision: requested=%d completed=%d", requested, completed)
	}
	// An old task must read again when a newer revision is known.
	check(2)
	if reads != 3 || estoqueDoProduto(t, id) != 7 {
		t.Fatalf("later change missing: reads=%d stock=%d", reads, estoqueDoProduto(t, id))
	}
	// Redelivery of the now-completed latest event reuses the verified read.
	check(4)
	if reads != 3 {
		t.Fatal("duplicate completion repeated GET")
	}
}

func TestTinyStockConcurrentReadDefersBeforeCallingERP(t *testing.T) {
	svc, row, _, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	started, finish := make(chan struct{}), make(chan struct{})
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(ctx context.Context, _ string) (int, error) {
			close(started)
			select {
			case <-finish:
				return 2, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}}, nil
	}})
	done := make(chan error, 1)
	go func() { _, err := svc.refreshERPAvailableStock(t.Context(), row, ext); done <- err }()
	<-started
	_, err := svc.refreshERPAvailableStock(t.Context(), row, ext)
	close(finish)
	if !errors.Is(err, errERPStockReadBusy) {
		t.Fatalf("overlap must retry without a second GET: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTinyStockRecoveryPrioritizesWaitingBuyersWithoutStarvingCatalog(t *testing.T) {
	svc, row, product, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	// Fill the old catalogue before introducing the waiting buyer's checkpoint.
	if _, err := testPool.Exec(t.Context(), `INSERT INTO products(store_id,name,keyword,external_source,external_id,price,stock)
  SELECT $1,'background-'||n,lpad(n::text,4,'0'),'tiny','background-'||n,1000,0 FROM generate_series(1,15) n`, row.StoreID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `INSERT INTO erp_stock_sync_state(product_id,last_attempt_at)
  SELECT id,now()-interval '2 days' FROM products WHERE store_id=$1`, row.StoreID); err != nil {
		t.Fatal(err)
	}
	var event string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,title,status,ends_at) VALUES($1,'waiting priority','ended',now()) RETURNING id::text`, row.StoreID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	seedQueueWaiter(t, scaleFixture{storeID: row.StoreID, eventID: event}, product, 1)
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_stock_sync_state SET last_attempt_at=now()-interval '1 hour' WHERE product_id=$1`, product); err != nil {
		t.Fatal(err)
	}
	// More urgent failures than the batch size must still leave two slots
	// for ordinary catalogue rotation and never overtake the waiting buyer.
	if _, err := testPool.Exec(t.Context(), `UPDATE erp_stock_sync_state s SET deferred_at=now() FROM products p WHERE p.id=s.product_id AND p.store_id=$1 AND p.external_id LIKE 'background-%' AND substring(p.external_id from 12)::int<=12`, row.StoreID); err != nil {
		t.Fatal(err)
	}
	ids, err := svc.claimTinyStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 10 {
		t.Fatalf("claim: %v %v", ids, err)
	}
	if ids[0] != ext {
		t.Fatalf("waiting buyer lost priority to old catalogue: %v", ids)
	}
	background := 0
	for _, id := range ids {
		if id == "background-13" || id == "background-14" || id == "background-15" {
			background++
		}
	}
	if background != 2 {
		t.Fatalf("ordinary catalogue starved by priority queue: %v", ids)
	}
	// The remaining capacity must still rotate through catalogue products.
	for _, id := range ids[1:] {
		if !strings.HasPrefix(id, "background-") {
			t.Fatalf("catalogue rotation missing: %v", ids)
		}
	}
}

func TestWaitlistWaitsForNewerStockInvalidation(t *testing.T) {
	svc, row, product, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	var event string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO live_events(store_id,title,status,ends_at) VALUES($1,'stock race','active',now()+interval '1 day') RETURNING id::text`, row.StoreID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	cart := seedQueueWaiter(t, scaleFixture{storeID: row.StoreID, eventID: event}, product, 1)
	if err := svc.enqueueTinyProductWebhook(t.Context(), row.StoreID, "estoque", ext); err != nil {
		t.Fatal(err)
	}
	promotion, err := testRepo.PromoteNextWaitlistEntry(t.Context(), row.StoreID, product)
	if !errors.Is(err, inventory.ErrWaitlistPromotionDeferred) || promotion != nil {
		t.Fatalf("promoted against known stale stock: %+v %v", promotion, err)
	}
	var waiting int
	if err := testPool.QueryRow(t.Context(), `SELECT waitlisted_quantity FROM cart_items WHERE cart_id=$1`, cart).Scan(&waiting); err != nil || waiting != 1 {
		t.Fatalf("waiting request changed: %d %v", waiting, err)
	}
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { return 1, nil }}, nil
	}})
	if _, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 1); err != nil {
		t.Fatal(err)
	}
	promotion, err = testRepo.PromoteNextWaitlistEntry(t.Context(), row.StoreID, product)
	if err != nil || promotion == nil || promotion.Quantity != 1 {
		t.Fatalf("verified restock did not promote buyer: %+v %v", promotion, err)
	}
}

func TestTinyStockFailedReadKeepsRevisionAndReleasesLease(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cancelDuringRead bool
	}{
		{name: "provider failure"},
		{name: "request cancelled", cancelDuringRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, row, product, ext := stockConsistencyFixture(t, syncedProduct)
			if err := svc.enqueueTinyProductWebhook(t.Context(), row.StoreID, "estoque", ext); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
				return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
					reads++
					if reads == 1 {
						if tc.cancelDuringRead {
							cancel()
							return 0, context.Canceled
						}
						return 0, errors.New("temporary provider failure")
					}
					return 4, nil
				}}, nil
			}})
			if applied, err := svc.refreshERPAvailableStockRevision(ctx, row, ext, 1); err == nil || applied {
				t.Fatalf("failed GET was accepted: applied=%v err=%v", applied, err)
			}
			var retained bool
			err := testPool.QueryRow(t.Context(), `SELECT requested_revision=1 AND completed_revision=0
				AND read_owner IS NULL AND read_until IS NULL AND last_success_at IS NULL
				FROM erp_stock_sync_state WHERE product_id=$1`, product).Scan(&retained)
			if err != nil || !retained || estoqueDoProduto(t, product) != 10 {
				t.Fatalf("failed read lost retry or changed stock: retained=%v err=%v", retained, err)
			}
			if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 1); err != nil || !applied {
				t.Fatalf("retry blocked by failed attempt: applied=%v err=%v", applied, err)
			}
			if reads != 2 || estoqueDoProduto(t, product) != 4 {
				t.Fatalf("retry did not apply latest stock: reads=%d stock=%d", reads, estoqueDoProduto(t, product))
			}
		})
	}
}

func TestTinyStockCompletedRevisionDoesNotBypassNewMerchantEdit(t *testing.T) {
	svc, row, product, ext := stockConsistencyFixture(t, syncedProduct)
	reads := 0
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { reads++; return 3, nil }}, nil
	}})
	if err := svc.enqueueTinyProductWebhook(t.Context(), row.StoreID, "estoque", ext); err != nil {
		t.Fatal(err)
	}
	if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 1); err != nil || !applied {
		t.Fatalf("initial stock read: %v %v", applied, err)
	}
	cart := seedStockPendingEdit(t, row.StoreID, product)
	if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 1); applied || !errors.Is(err, errERPStockPendingEdit) {
		t.Fatalf("old completion bypassed newer pending edit: %v %v", applied, err)
	}
	if reads != 1 || estoqueDoProduto(t, product) != 3 {
		t.Fatal("pending edit triggered another GET or overwrote stock")
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET synced_revision=revision WHERE cart_id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	if applied, err := svc.refreshERPAvailableStockRevision(t.Context(), row, ext, 1); err != nil || !applied || reads != 2 {
		t.Fatalf("deferred edit must require a fresh read: applied=%v reads=%d err=%v", applied, reads, err)
	}
}

func TestTinyStockRefreshDoesNotReofferUnacknowledgedUnit(t *testing.T) {
	for _, duringRead := range []bool{false, true} {
		name := "reservation still travelling to ERP"
		if duringRead {
			name = "ERP acknowledgement during GET"
		}
		t.Run(name, func(t *testing.T) {
			svc, row, product, ext := stockConsistencyFixture(t, syncedProduct)
			var cart string
			err := testPool.QueryRow(t.Context(), `UPDATE carts SET payment_status='pending',paid_amount_cents=0,
				purchase_closed=false,erp_order_state='open',erp_order_status='aberto'
				WHERE store_id=$1 RETURNING id::text`, row.StoreID).Scan(&cart)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE cart_items SET quantity=2,
				erp_confirmed_quantity=1,erp_pending_since=now() WHERE cart_id=$1`, cart); err != nil {
				t.Fatal(err)
			}
			// The local reservation is already deducted, while the ERP HTTP
			// write has not yet arrived. Its GET still reports the previous ten.
			if err := testRepo.DecrementProductStock(t.Context(), product, 1); err != nil {
				t.Fatal(err)
			}
			ack := func() {
				t.Helper()
				if err := testRepo.ConfirmERPGrid(t.Context(), cart,
					[]providers.ERPOrderItem{{ProductID: ext, Quantity: 2, UnitPrice: 1000}}); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
				return tinyStockReaderStub{read: func(context.Context, string) (int, error) {
					reads++
					if reads == 1 {
						if duringRead {
							ack()
						}
						return 10, nil
					}
					return 9, nil
				}}, nil
			}})
			applied, err := svc.refreshERPAvailableStock(t.Context(), row, ext)
			if duringRead {
				if err == nil || applied {
					t.Fatalf("GET predating acknowledgement was applied: %v %v", applied, err)
				}
			} else if err != nil || !applied {
				t.Fatalf("valid balance minus pending promise was rejected: %v %v", applied, err)
			}
			if got := estoqueDoProduto(t, product); got != 9 {
				t.Fatalf("reserved unit returned to stock: got=%d want=9", got)
			}
			if !duringRead {
				ack()
			}
			if applied, err := svc.refreshERPAvailableStock(t.Context(), row, ext); err != nil || !applied {
				t.Fatalf("confirmed fresh balance was rejected: %v %v", applied, err)
			}
			if got := estoqueDoProduto(t, product); got != 9 {
				t.Fatalf("confirmed promise was deducted twice: got=%d want=9", got)
			}
		})
	}
}
