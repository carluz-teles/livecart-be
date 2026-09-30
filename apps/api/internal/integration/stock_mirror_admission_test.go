//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/live"
)

func admitStockMirrorBuyer(t *testing.T, store, product, buyer string) string {
	t.Helper()
	var event string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM live_events WHERE store_id=$1 LIMIT 1`, store).Scan(&event); err != nil {
		t.Fatal(err)
	}
	comments := live.NewService(live.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	result, err := comments.ApplyCommentItem(t.Context(), live.AddToCartInput{
		StoreID: store, EventID: event, ProductID: product, ProductPrice: 1000,
		PlatformUserID: buyer, PlatformHandle: buyer, Quantity: 1,
	}, product+":"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	if result.WaitlistedQuantity != 0 {
		t.Fatal("fixture did not reserve the requested unit")
	}
	return result.CartID
}

func TestStockMirrorProductionFlowPreservesPendingAndAcknowledgedUnits(t *testing.T) {
	svc, row, product, external := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) {
		t.Fatal("unexpected product detail request")
		return nil, nil
	})
	available := 10
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { return available, nil }}, nil
	}})
	cart := admitStockMirrorBuyer(t, row.StoreID, product, "pending-buyer")
	assertRefresh := func(want int) {
		t.Helper()
		applied, err := svc.refreshERPAvailableStock(t.Context(), row, external)
		if err != nil || !applied || estoqueDoProduto(t, product) != want {
			t.Fatalf("ERP=%d applied=%v err=%v local=%d want=%d", available, applied, err, estoqueDoProduto(t, product), want)
		}
	}
	assertRefresh(9) // The old ERP balance cannot restore the pending unit.
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='ack-order',erp_order_state='open',erp_order_status='aberto' WHERE id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.ConfirmERPGrid(t.Context(), cart, []providers.ERPOrderItem{{ProductID: external, Quantity: 1, UnitPrice: 1000}}); err != nil {
		t.Fatal(err)
	}
	available = 9
	assertRefresh(9) // The acknowledged unit must not be deducted twice.
	available = 8
	assertRefresh(8) // A sale in another channel still lowers availability.
	available = 12
	assertRefresh(12) // Genuine replenishment must remain possible.
}

func TestStockMirrorProductionFlowInvalidatesReadCrossingAcknowledgement(t *testing.T) {
	svc, row, product, external := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	cart := admitStockMirrorBuyer(t, row.StoreID, product, "racing-buyer")
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='ack-order',erp_order_state='open',erp_order_status='aberto' WHERE id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(ctx context.Context, _ string) (int, error) {
			// The GET took its snapshot before the pending order reached Tiny,
			// but its response arrives after the grid was acknowledged locally.
			err := testRepo.ConfirmERPGrid(ctx, cart, []providers.ERPOrderItem{{ProductID: external, Quantity: 1, UnitPrice: 1000}})
			return 10, err
		}}, nil
	}})
	applied, err := svc.refreshERPAvailableStock(t.Context(), row, external)
	if applied || err == nil || estoqueDoProduto(t, product) != 9 {
		t.Fatalf("old response accepted after ACK: applied=%v err=%v stock=%d", applied, err, estoqueDoProduto(t, product))
	}
}

func TestConcurrentCommentReservationsSurviveProductionStockRefresh(t *testing.T) {
	svc, row, product, external := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=200 WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { admitStockMirrorBuyer(t, row.StoreID, product, fmt.Sprintf("parallel-%d", i)) })
	}
	wg.Wait()
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { return 180, nil }}, nil
	}})
	// Twenty external sales and fifty locally admitted units not yet sent.
	if applied, err := svc.refreshERPAvailableStock(t.Context(), row, external); err != nil || !applied || estoqueDoProduto(t, product) != 130 {
		t.Fatalf("concurrent reservations lost: applied=%v err=%v stock=%d want=130", applied, err, estoqueDoProduto(t, product))
	}
}

func TestBlingDeferredStockRecoversAfterCartEdit(t *testing.T) {
	svc, row, product, external := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) { return nil, nil })
	if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET provider='bling' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET external_source='bling' WHERE id=$1`, product); err != nil {
		t.Fatal(err)
	}
	row.Provider = "bling"
	cart := seedStockPendingEdit(t, row.StoreID, product)
	reads := 0
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), BlingConstructor: func(providers.BlingConfig) (providers.ERPProvider, error) {
		return tinyStockReaderStub{read: func(context.Context, string) (int, error) { reads++; return 4, nil }}, nil
	}})
	if _, err := svc.refreshERPAvailableStock(t.Context(), row, external); err == nil {
		t.Fatal("pending edit did not defer Bling refresh")
	}
	if reads != 0 {
		t.Fatal("read ERP while protected edit was pending")
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_erp_edits SET synced_revision=revision WHERE cart_id=$1`, cart); err != nil {
		t.Fatal(err)
	}
	ids, err := svc.claimERPStockChecks(t.Context(), row.StoreID)
	if err != nil || len(ids) != 1 || ids[0] != external {
		t.Fatalf("Bling recovery claims=%v err=%v", ids, err)
	}
	if applied, err := svc.refreshERPAvailableStock(t.Context(), row, ids[0]); err != nil || !applied {
		t.Fatalf("Bling recovery applied=%v err=%v", applied, err)
	}
	var pending bool
	if err := testPool.QueryRow(t.Context(), `SELECT deferred_at IS NOT NULL FROM erp_stock_sync_state WHERE product_id=$1`, product).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	// Local reservation mode also deducts the fixture's unlaunched held unit.
	if pending || reads != 1 || estoqueDoProduto(t, product) != 3 {
		t.Fatalf("Bling recovery pending=%v reads=%d stock=%d", pending, reads, estoqueDoProduto(t, product))
	}
}
