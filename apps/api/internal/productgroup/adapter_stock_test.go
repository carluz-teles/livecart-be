//go:build integration

package productgroup

import (
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
	"testing"
)

func TestVariantMetadataSyncPreservesConcurrentStockReservation(t *testing.T) {
	requireDB(t)
	store := seedStore(t)
	catalog := product.NewService(product.NewRepository(testQueries, testPool), zap.NewNop())
	adapter := NewSyncerAdapter(testSvc, catalog)
	parent := providers.ERPProduct{ID: "parent", Name: "Caneca", IsParent: true, GradeKeys: []string{"Cor"}, Variants: []providers.ERPProduct{
		{ID: "child", Name: "Azul", Price: 1000, Stock: 5, StockKnown: true, Active: true, Attributes: map[string]string{"Cor": "Azul"}},
	}}
	if err := adapter.SyncFromERP(t.Context(), store.String(), "tiny", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=4,erp_seq=erp_seq+1 WHERE store_id=$1 AND external_id='child'`, store.String()); err != nil {
		t.Fatal(err)
	}
	parent.Variants[0].Price = 1100
	if err := adapter.SyncFromERP(t.Context(), store.String(), "tiny", parent); err != nil {
		t.Fatal(err)
	}
	var stock, price int
	if err := testPool.QueryRow(t.Context(), `SELECT stock,price FROM products WHERE store_id=$1 AND external_id='child'`, store.String()).Scan(&stock, &price); err != nil {
		t.Fatal(err)
	}
	if stock != 4 || price != 1100 {
		t.Fatalf("stock=%d price=%d; stale stock was admitted or metadata lost", stock, price)
	}
}
