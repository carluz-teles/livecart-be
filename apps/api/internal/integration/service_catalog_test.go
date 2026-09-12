//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
)

type catalogSyncProvider struct {
	providers.ERPProvider
	product providers.ERPProduct
}

func (p catalogSyncProvider) GetProduct(context.Context, string) (*providers.ERPProduct, error) {
	copy := p.product
	return &copy, nil
}

// Fail only the reservation calculation; catalog reads/writes still work.
type catalogStockReadFailure struct{ sqlc.DBTX }

func (db catalogStockReadFailure) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.HasPrefix(query, "-- name: SumPromisedNotYetReflected") {
		return db.DBTX.QueryRow(ctx, "SELECT 1/0")
	}
	return db.DBTX.QueryRow(ctx, query, args...)
}

func TestProcessProductSyncBackfillsIdentifiersWhenStockCalculationFails(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedPaidCart(t, 1, 0)
	svc := reconnectTestService(t)
	svc.repo = NewRepository(sqlc.New(catalogStockReadFailure{DBTX: testPool}), testPool)
	catalog := product.NewService(product.NewRepository(sqlc.New(testPool), testPool), zap.NewNop())
	svc.productSyncer = product.NewProductSyncerAdapter(catalog)
	var externalID string
	if err := testPool.QueryRow(ctx, `SELECT external_id FROM products WHERE id=$1`, fx.productID).Scan(&externalID); err != nil {
		t.Fatal(err)
	}
	svc.factory = providers.NewFactory(providers.FactoryConfig{
		Logger: zap.NewNop(),
		TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) {
			return catalogSyncProvider{product: providers.ERPProduct{
				ID: externalID, Name: "Galho", SKU: "47169001", GTIN: "7893979655073",
				Price: 7990, Stock: 50, Active: true,
			}}, nil
		},
	})
	encrypted, err := svc.encryptor.EncryptJSON(providers.Credentials{
		AccessToken: "local-test", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &IntegrationRow{StoreID: fx.storeID, Provider: "tiny", Type: "erp", Credentials: encrypted}
	before, err := svc.repo.catalogIdentifierCoverage(ctx, fx.storeID, "tiny")
	if err != nil || before.missingSKU != 1 || before.missingBarcode != 1 {
		t.Fatalf("unexpected initial coverage: %+v, %v", before, err)
	}
	outcome, err := svc.processProductSync(ctx, row, externalID)
	if err != nil || outcome != stockMirrorStale {
		t.Fatalf("stock calculation failure should defer stock: outcome=%v err=%v", outcome, err)
	}
	var sku, barcode string
	var stock int
	if err := testPool.QueryRow(ctx, `SELECT sku, barcode, stock FROM products WHERE id=$1`, fx.productID).
		Scan(&sku, &barcode, &stock); err != nil {
		t.Fatal(err)
	}
	if sku != "47169001" || barcode != "7893979655073" || stock != 10 {
		t.Fatalf("metadata must be synced without admitting stock: sku=%q barcode=%q stock=%d", sku, barcode, stock)
	}
	after, err := svc.repo.catalogIdentifierCoverage(ctx, fx.storeID, "tiny")
	if err != nil || after.total != 1 || after.missingSKU != 0 || after.missingBarcode != 0 {
		t.Fatalf("unexpected final coverage: %+v, %v", after, err)
	}
}
