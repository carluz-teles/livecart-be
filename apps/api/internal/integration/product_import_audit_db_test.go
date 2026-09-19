//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product"
	"livecart/apps/api/internal/productgroup"
	"livecart/apps/api/lib/httpx"
)

type auditCatalogProvider struct {
	providers.ERPProvider
	read   func(string) (*providers.ERPProduct, error)
	parent *providers.ERPProduct
}

func (p *auditCatalogProvider) Name() providers.ProviderName { return providers.ProviderTiny }
func (p *auditCatalogProvider) GetProduct(_ context.Context, id string) (*providers.ERPProduct, error) {
	return p.read(id)
}
func (p *auditCatalogProvider) ListProducts(context.Context, providers.ListProductsParams) (*providers.ProductListResult, error) {
	return &providers.ProductListResult{Products: []providers.ERPProduct{*p.parent}}, nil
}
func auditCatalogFixture(t *testing.T, p *auditCatalogProvider) (*Service, *IntegrationRow) {
	t.Helper()
	svc, row, _, _ := stockConsistencyFixture(t, p.read)
	svc.factory = providers.NewFactory(providers.FactoryConfig{Logger: zap.NewNop(), TinyConstructor: func(providers.TinyConfig) (providers.ERPProvider, error) { return p, nil }})
	catalog := product.NewService(product.NewRepository(svc.repo.queries, testPool), zap.NewNop())
	catalog.SetERPProductReader(svc.ReadCatalogProduct)
	svc.productGroupSyncer = productgroup.NewSyncerAdapter(productgroup.NewService(productgroup.NewRepository(svc.repo.queries, testPool), zap.NewNop()), catalog)
	return svc, row
}
func TestAuditImportRechecksActiveAndKnownStock(t *testing.T) {
	for _, tc := range []struct {
		name          string
		active, known bool
		stock         int
		status        int
	}{
		{"unknown available", true, false, 0, 503},
		{"became inactive", false, true, 3, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &auditCatalogProvider{read: func(string) (*providers.ERPProduct, error) {
				return &providers.ERPProduct{ID: "audit-product", Name: "Audit", SKU: "000123", GTIN: "7893979655073", Price: 1000, Stock: tc.stock, StockKnown: tc.known, Active: tc.active}, nil
			}}
			svc, row := auditCatalogFixture(t, p)
			out, err := svc.ImportERPProduct(t.Context(), ImportERPProductInput{StoreID: row.StoreID, IntegrationID: row.ID, TinyProductID: "audit-product"})
			if err == nil {
				var stock int
				var active bool
				if e := testPool.QueryRow(t.Context(), `SELECT stock,active FROM products WHERE id=$1`, out.ProductID).Scan(&stock, &active); e != nil {
					t.Fatal(e)
				}
				t.Errorf("BUG: import succeeded despite %s: saved stock=%d active=%v", tc.name, stock, active)
			} else if httpx.StatusFromError(err) != tc.status {
				t.Errorf("wrong rejection: %v", err)
			}
		})
	}
}
func TestAuditPartialGroupCanImportRemainingVariant(t *testing.T) {
	parent := providers.ERPProduct{ID: "audit-parent", Name: "Caneca", IsParent: true, Active: true, GradeKeys: []string{"Cor"}, Variants: []providers.ERPProduct{
		{ID: "audit-blue", Name: "Azul", SKU: "BLUE", Price: 1000, Stock: 3, StockKnown: true, Active: true, Attributes: map[string]string{"Cor": "Azul"}},
		{ID: "audit-red", Name: "Vermelha", SKU: "RED", Price: 1000, Stock: 4, StockKnown: true, Active: true, Attributes: map[string]string{"Cor": "Vermelha"}},
	}}
	p := &auditCatalogProvider{parent: &parent, read: func(id string) (*providers.ERPProduct, error) {
		if id == parent.ID {
			copy := parent
			copy.Variants = append([]providers.ERPProduct(nil), parent.Variants...)
			return &copy, nil
		}
		for _, v := range parent.Variants {
			if v.ID == id {
				return &v, nil
			}
		}
		panic("unexpected audit id")
	}}
	svc, row := auditCatalogFixture(t, p)
	input := ImportERPProductInput{StoreID: row.StoreID, IntegrationID: row.ID, TinyProductID: parent.ID, VariantIDs: []string{"audit-blue"}}
	if _, err := svc.ImportERPProduct(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.SearchProducts(t.Context(), SearchProductsInput{StoreID: row.StoreID, IntegrationID: row.ID, Search: "Caneca", SummaryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !listed.Products[0].GroupImported || listed.Products[0].AlreadyImported {
		t.Fatalf("partial group not offered for continuation: %+v", listed.Products[0])
	}
	input.VariantIDs = []string{"audit-red"}
	second, err := svc.ImportERPProduct(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Imported) != 1 || second.Imported[0].ExternalID != "audit-red" {
		t.Fatalf("wrong imported summary: %+v", second)
	}
	retry, err := svc.ImportERPProduct(t.Context(), input)
	if err != nil || len(retry.Imported) != 0 || retry.GroupID != second.GroupID {
		t.Fatalf("retry is not idempotent: %+v %v", retry, err)
	}
	var groups, products, options, values int
	err = testPool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM product_groups WHERE store_id=$1), (SELECT count(*) FROM products WHERE group_id=$2), (SELECT count(*) FROM product_options WHERE group_id=$2), (SELECT count(*) FROM product_option_values WHERE option_id IN (SELECT id FROM product_options WHERE group_id=$2))`, row.StoreID, second.GroupID).Scan(&groups, &products, &options, &values)
	if err != nil {
		t.Fatal(err)
	}
	if groups != 1 || products != 2 || options != 1 || values != 2 {
		t.Fatalf("duplicate or missing rows: %d %d %d %d", groups, products, options, values)
	}
}
func TestAuditImportTenantIsolation(t *testing.T) {
	calls := 0
	p := &auditCatalogProvider{read: func(string) (*providers.ERPProduct, error) { calls++; return nil, nil }}
	svc, row := auditCatalogFixture(t, p)
	_, err := svc.ImportERPProduct(t.Context(), ImportERPProductInput{StoreID: uuid.NewString(), IntegrationID: row.ID, TinyProductID: "123"})
	if err == nil || calls != 0 {
		t.Fatalf("cross-store ERP access: calls=%d err=%v", calls, err)
	}
	t.Logf("Cross-store request rejected before ERP access: %v", err)
}

func TestAuditSimpleFormRevalidatesBeforeSaving(t *testing.T) {
	available := 5
	p := &auditCatalogProvider{read: func(id string) (*providers.ERPProduct, error) {
		return &providers.ERPProduct{ID: id, Name: "Audit simple", SKU: "000123", GTIN: "7893979655073", Active: true, StockKnown: true, Stock: available, Price: 1000}, nil
	}}
	svc, row := auditCatalogFixture(t, p)
	selected, err := svc.GetERPProductDetails(t.Context(), row.StoreID, row.ID, "audit-simple")
	if err != nil {
		t.Fatal(err)
	}
	available = 0 // sold in Tiny after preview, before the form is submitted
	request := product.CreateProductRequest{Name: selected.Name, ExternalID: selected.ID, ExternalSource: "tiny", Price: selected.Price, Stock: selected.Stock, Shipping: product.ShippingProfileDTO{SKU: selected.SKU, Barcode: selected.GTIN}}
	input, err := request.ToInput(row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	catalog := product.NewService(product.NewRepository(svc.repo.queries, testPool), zap.NewNop())
	catalog.SetERPProductReader(svc.ReadCatalogProduct)
	saved, err := catalog.Create(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Shipping().SKU != "000123" || saved.Shipping().Barcode != "7893979655073" {
		t.Fatal("identifier lost")
	}
	if saved.Stock() != available {
		t.Errorf("BUG: current ERP available=%d; form saved stale stock=%d", available, saved.Stock())
	}
}

func TestAuditDuplicateSimpleFormIsConflict(t *testing.T) {
	svc, row := auditCatalogFixture(t, &auditCatalogProvider{read: func(id string) (*providers.ERPProduct, error) {
		return &providers.ERPProduct{ID: id, Active: true, StockKnown: true, Stock: 2}, nil
	}})
	catalog := product.NewService(product.NewRepository(svc.repo.queries, testPool), zap.NewNop())
	catalog.SetERPProductReader(svc.ReadCatalogProduct)
	input, err := (product.CreateProductRequest{Name: "Audit duplicate", ExternalSource: "tiny", ExternalID: "audit-duplicate", Price: 1000, Stock: 2}).ToInput(row.StoreID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Create(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	_, err = catalog.Create(t.Context(), input)
	if err == nil {
		t.Fatal("duplicate created")
	}
	status, _ := auditWire(t, err)
	var count int
	if e := testPool.QueryRow(t.Context(), `SELECT count(*) FROM products WHERE store_id=$1 AND external_id='audit-duplicate'`, row.StoreID).Scan(&count); e != nil {
		t.Fatal(e)
	}
	if status != 409 || count != 1 {
		t.Errorf("BUG: duplicate prevented but status=%d (want 409); rows=%d", status, count)
	}
}

func TestAuditConcurrentGroupImportIsIdempotent(t *testing.T) {
	p := &auditCatalogProvider{read: func(id string) (*providers.ERPProduct, error) {
		v := providers.ERPProduct{ID: "audit-concurrent-child", SKU: "000007", GTIN: "7893979655073", Name: "Azul", Active: true, StockKnown: true, Stock: 3, Price: 1000, Attributes: map[string]string{"Cor": "Azul"}}
		if id == v.ID {
			return &v, nil
		}
		return &providers.ERPProduct{ID: id, Name: "Caneca", Active: true, IsParent: true, GradeKeys: []string{"Cor"}, Variants: []providers.ERPProduct{v}}, nil
	}}
	svc, row := auditCatalogFixture(t, p)
	results := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := svc.ImportERPProduct(t.Context(), ImportERPProductInput{StoreID: row.StoreID, IntegrationID: row.ID, TinyProductID: "audit-concurrent-parent", VariantIDs: []string{"audit-concurrent-child"}})
			results <- err
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM products WHERE store_id=$1 AND external_id='audit-concurrent-child'`, row.StoreID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent import created %d products", count)
	}
}

func TestAuditVariantStockFailureLeavesNoPartialGroup(t *testing.T) {
	p := &auditCatalogProvider{read: func(id string) (*providers.ERPProduct, error) {
		v := providers.ERPProduct{ID: "audit-unknown-child", Name: "Azul", Active: true, StockKnown: true, Stock: 3, Price: 1000, Attributes: map[string]string{"Cor": "Azul"}}
		if id == v.ID {
			v.StockKnown = false
			v.Stock = 0
			return &v, nil
		}
		return &providers.ERPProduct{ID: id, Name: "Caneca", Active: true, IsParent: true, GradeKeys: []string{"Cor"}, Variants: []providers.ERPProduct{v}}, nil
	}}
	svc, row := auditCatalogFixture(t, p)
	_, err := svc.ImportERPProduct(t.Context(), ImportERPProductInput{StoreID: row.StoreID, IntegrationID: row.ID, TinyProductID: "audit-unknown-parent"})
	if httpx.StatusFromError(err) != 503 {
		t.Fatalf("unknown stock accepted: %v", err)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM product_groups WHERE store_id=$1`, row.StoreID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed batch left %d groups", count)
	}
}
