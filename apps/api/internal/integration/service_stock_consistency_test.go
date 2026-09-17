//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/lib/httpx"
)

type stockConsistencyProvider struct {
	providers.ERPProvider
	read func() (*providers.ERPProduct, error)
}

func (p stockConsistencyProvider) GetProduct(context.Context, string) (*providers.ERPProduct, error) {
	return p.read()
}

func stockConsistencyFixture(t *testing.T, read func(string) (*providers.ERPProduct, error)) (*Service, *IntegrationRow, string, string) {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='known',erp_order_status='faturado' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	svc := reconnectTestService(t)
	svc.productSyncer = product.NewProductSyncerAdapter(product.NewService(product.NewRepository(svc.repo.queries, testPool), zap.NewNop()))
	var external string
	if err := testPool.QueryRow(t.Context(), `SELECT external_id FROM products WHERE id=$1`, fx.productID).Scan(&external); err != nil {
		t.Fatal(err)
	}
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
		return stockConsistencyProvider{read: func() (*providers.ERPProduct, error) { return read(fx.productID) }}, nil
	}})
	encrypted, err := svc.encryptor.EncryptJSON(providers.Credentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := testPool.QueryRow(t.Context(), `UPDATE integrations SET credentials=$2 WHERE store_id=$1 AND provider='tiny' RETURNING id::text`, fx.storeID, encrypted).Scan(&id); err != nil {
		t.Fatal(err)
	}
	row, err := svc.repo.GetByID(t.Context(), id, fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	return svc, row, fx.productID, external
}

func TestStockMirrorRejectsOlderConcurrentSnapshot(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	seq := seqDoProduto(t, fx.productID)
	applied, err := testRepo.ApplyERPStockMirror(t.Context(), fx.productID, 0, seq)
	if err != nil || !applied {
		t.Fatalf("fresh zero: %v %v", applied, err)
	}
	applied, err = testRepo.ApplyERPStockMirror(t.Context(), fx.productID, 2, seq)
	if err != nil || applied || estoqueDoProduto(t, fx.productID) != 0 {
		t.Fatalf("old balance replaced fresh zero: %v %v", applied, err)
	}
}

func TestManualStockSyncDoesNotReofferConcurrentPurchase(t *testing.T) {
	svc, row, id, _ := stockConsistencyFixture(t, func(id string) (*providers.ERPProduct, error) {
		if err := testRepo.DecrementProductStock(t.Context(), id, 1); err != nil {
			t.Fatal(err)
		}
		return &providers.ERPProduct{ID: "ignored", Name: "Product", Price: 1000, Stock: 10, StockKnown: true, Active: true}, nil
	})
	// A concurrent reservation invalidates this snapshot. The UI must be told
	// to retry, and the unit cannot become available again.
	_, err := svc.SyncProductManual(t.Context(), SyncProductInput{StoreID: row.StoreID, IntegrationID: row.ID, ProductID: id})
	if httpx.StatusFromError(err) != 409 {
		t.Fatalf("stale sync error = %v", err)
	}
	if got := estoqueDoProduto(t, id); got != 9 {
		t.Fatalf("available=%d, want 9", got)
	}
}

func TestUnknownAvailableStockDoesNotBecomeZero(t *testing.T) {
	var external string
	svc, row, id, ext := stockConsistencyFixture(t, func(string) (*providers.ERPProduct, error) {
		return &providers.ERPProduct{ID: external, Name: "Product", Price: 1000, Active: true, StockKnown: false}, nil
	})
	external = ext
	outcome, err := svc.processProductSync(t.Context(), row, ext)
	if outcome == stockMirrorApplied || err == nil || estoqueDoProduto(t, id) != 10 {
		t.Fatalf("unknown stock accepted: outcome=%v err=%v stock=%d", outcome, err, estoqueDoProduto(t, id))
	}
}
