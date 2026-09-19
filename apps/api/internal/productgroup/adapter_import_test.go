//go:build integration

package productgroup

import (
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/lib/httpx"
)

func TestResumedERPImportPreservesExistingProductsAndGradeUniqueness(t *testing.T) {
	requireDB(t)
	store := seedStore(t)
	catalog := product.NewService(product.NewRepository(testQueries, testPool), zap.NewNop())
	adapter := NewSyncerAdapter(testSvc, catalog)
	parent := providers.ERPProduct{ID: "parent", Name: "Caneca", Active: true, IsParent: true, GradeKeys: []string{"Cor"}, Variants: []providers.ERPProduct{
		{ID: "blue", Active: true, StockKnown: true, Stock: 5, Price: 1000, Attributes: map[string]string{"Cor": "Azul"}},
	}}
	groupID, _, err := adapter.ImportFromERP(t.Context(), store.String(), "tiny", parent)
	if err != nil {
		t.Fatal(err)
	}
	// A retry with a different snapshot must preserve the previously saved stock.
	parent.Variants[0].Stock = 99
	parent.Variants[0].Price = 9999
	sameGroup, imported, err := adapter.ImportFromERP(t.Context(), store.String(), "tiny", parent)
	if err != nil || sameGroup != groupID || len(imported) != 0 {
		t.Fatalf("retry: %s %+v %v", sameGroup, imported, err)
	}
	var stock, price int
	if err := testPool.QueryRow(t.Context(), `SELECT stock,price FROM products WHERE store_id=$1 AND external_id='blue'`, store.String()).Scan(&stock, &price); err != nil {
		t.Fatal(err)
	}
	if stock != 5 || price != 1000 {
		t.Fatalf("retry overwrote catalogue: stock=%d price=%d", stock, price)
	}
	// A different ERP identity cannot introduce a second identical option combination.
	parent.Variants[0].ID = "duplicate-blue"
	_, _, err = adapter.ImportFromERP(t.Context(), store.String(), "tiny", parent)
	if httpx.StatusFromError(err) != 422 {
		t.Fatalf("duplicate grade accepted: %v", err)
	}
}
