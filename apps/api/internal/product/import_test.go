package product

import (
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/product/domain"
	vo "livecart/apps/api/lib/valueobject"
)

func importPtr[T any](value T) *T { return &value }
func importProduct(t *testing.T, sid vo.StoreID, code, sku string, external bool) *domain.Product {
	t.Helper()
	source := domain.ExternalSourceManual
	if external {
		source = domain.ExternalSourceTiny
	}
	money, _ := vo.NewMoney(1234)
	p, err := domain.NewProduct(sid, "Original", "", source, domain.MustKeyword(code), money, "private/key", 7, domain.ShippingProfile{SKU: sku, Barcode: "0012345"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestImportPlanPreservesOmittedFieldsAndZero(t *testing.T) {
	sid := vo.GenerateStoreID()
	p := importProduct(t, sid, "1000", "0001", false)
	req := ImportProductsRequest{MatchBy: "keyword", Rows: []ImportProductRow{{Row: 2, Keyword: "1000", Price: importPtr(int64(0)), Active: importPtr(false)}}}
	plan := planProductImport(sid, req, []*domain.Product{p})
	got := plan.products[0]
	if plan.response.Invalid != 0 || plan.response.Updated != 1 || got.Price().Cents() != 0 || got.Stock() != 7 || got.Name() != "Original" || got.ImageURL() != "private/key" || got.Shipping().Barcode != "0012345" || got.Active() {
		t.Fatalf("plan=%+v product=%+v", plan.response, got)
	}
	if p.Price().Cents() != 1234 {
		t.Fatal("preview mutated source snapshot")
	}
	again := planProductImport(sid, req, []*domain.Product{p})
	if again.response.Revision != plan.response.Revision {
		t.Fatal("unstable revision")
	}
}
func TestImportPlanProtectsERPStock(t *testing.T) {
	sid := vo.GenerateStoreID()
	p := importProduct(t, sid, "1000", "0001", true)
	req := ImportProductsRequest{MatchBy: "sku", Rows: []ImportProductRow{{Row: 2, SKU: importPtr("0001"), Stock: importPtr(99), Name: importPtr("Novo nome")}}}
	plan := planProductImport(sid, req, []*domain.Product{p})
	if plan.response.Invalid != 0 || plan.products[0].Stock() != 7 || len(plan.response.Rows[0].Warnings) != 2 || plan.products[0].ExternalSource() != p.ExternalSource() {
		t.Fatalf("%+v", plan.response)
	}
}
func TestImportPlanRejectsAmbiguityAndInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rows         []ImportProductRow
		match        string
		duplicateSKU bool
	}{
		{"repeated code", []ImportProductRow{{Row: 2, Keyword: "1000"}, {Row: 3, Keyword: "1000"}}, "keyword", false},
		{"ambiguous SKU", []ImportProductRow{{Row: 2, SKU: importPtr("same")}}, "sku", true},
		{"cross identifiers", []ImportProductRow{{Row: 2, SKU: importPtr("same"), Keyword: "1001"}}, "sku", false},
		{"new needs price", []ImportProductRow{{Row: 2, Keyword: "1002", Name: importPtr("Novo")}}, "keyword", false},
		{"negative stock", []ImportProductRow{{Row: 2, Keyword: "1000", Stock: importPtr(-1)}}, "keyword", false},
		{"invalid code", []ImportProductRow{{Row: 2, Keyword: "ABC", Price: importPtr(int64(0)), Name: importPtr("Novo")}}, "keyword", false},
		{"two SKUs same new code", []ImportProductRow{{Row: 2, Keyword: "2000", SKU: importPtr("a"), Name: importPtr("A"), Price: importPtr(int64(10))}, {Row: 3, Keyword: "2000", SKU: importPtr("b"), Name: importPtr("B"), Price: importPtr(int64(10))}}, "sku", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sid := vo.GenerateStoreID()
			catalog := []*domain.Product{importProduct(t, sid, "1000", "same", false)}
			if tc.duplicateSKU {
				catalog = append(catalog, importProduct(t, sid, "1001", "same", false))
			}
			p := planProductImport(sid, ImportProductsRequest{MatchBy: tc.match, Rows: tc.rows}, catalog)
			if p.response.Invalid == 0 {
				t.Fatalf("accepted invalid import: %+v", p.response)
			}
		})
	}
}
func TestImportSKUCodeAllocationReservesExplicitCodes(t *testing.T) {
	sid := vo.GenerateStoreID()
	req := ImportProductsRequest{MatchBy: "sku", Rows: []ImportProductRow{
		{Row: 2, SKU: importPtr("a"), Name: importPtr("A"), Price: importPtr(int64(100))},
		{Row: 3, SKU: importPtr("b"), Keyword: "1000", Name: importPtr("B"), Price: importPtr(int64(0))}}}
	plan := planProductImport(sid, req, nil)
	if plan.response.Invalid != 0 || plan.response.Rows[0].Keyword != "1001" || plan.response.Rows[1].Keyword != "1000" {
		t.Fatalf("%+v", plan.response)
	}
}
func TestImportProductsAtomicTenantSafeAndReplay(t *testing.T) {
	requireBuscaDB(t)
	sid := semearCatalogo(t)
	other := semearCatalogo(t)
	svc := NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop())
	req := ImportProductsRequest{MatchBy: "sku", Rows: []ImportProductRow{{Row: 2, SKU: importPtr("000-new"), Name: importPtr("Planilha"), Price: importPtr(int64(0)), Stock: importPtr(3), Active: importPtr(false)}}}
	preview, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), false)
	if err != nil || preview.Created != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	req.Revision = preview.Revision
	if _, err := svc.ImportProducts(t.Context(), testImportInput(t, other, req), true); err == nil {
		t.Fatal("accepted revision from another tenant")
	}
	result, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), true)
	if err != nil || !result.Applied {
		t.Fatalf("apply=%+v err=%v", result, err)
	}
	if _, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), true); err == nil {
		t.Fatal("stale retry should request new preview")
	}
	preview, err = svc.ImportProducts(t.Context(), testImportInput(t, sid, req), false)
	if err != nil || preview.Created != 0 || preview.Updated != 1 {
		t.Fatalf("retry duplicated product: %+v %v", preview, err)
	}
	var count, stock int
	var active bool
	if err := buscaPool.QueryRow(t.Context(), `SELECT count(*),max(stock),bool_or(active) FROM products WHERE store_id=$1 AND sku='000-new'`, sid.String()).Scan(&count, &stock, &active); err != nil {
		t.Fatal(err)
	}
	if count != 1 || stock != 3 || active {
		t.Fatalf("count=%d stock=%d active=%v", count, stock, active)
	}
	// Invalid later row must prevent the earlier valid update as well.
	req.Rows = append(req.Rows, ImportProductRow{Row: 3, SKU: importPtr("bad-new")})
	req.Rows[0].Name = importPtr("Should not save")
	preview, err = svc.ImportProducts(t.Context(), testImportInput(t, sid, req), false)
	if err != nil {
		t.Fatal(err)
	}
	req.Revision = preview.Revision
	if _, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), true); err == nil {
		t.Fatal("accepted invalid batch")
	}
	var name string
	if err := buscaPool.QueryRow(t.Context(), `SELECT name FROM products WHERE store_id=$1 AND sku='000-new'`, sid.String()).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Planilha" {
		t.Fatal("partial import")
	}
}
func TestImportProductsRejectsChangedStock(t *testing.T) {
	requireBuscaDB(t)
	sid := semearCatalogo(t)
	svc := NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop())
	var code string
	if err := buscaPool.QueryRow(t.Context(), `SELECT keyword FROM products WHERE store_id=$1 LIMIT 1`, sid.String()).Scan(&code); err != nil {
		t.Fatal(err)
	}
	req := ImportProductsRequest{MatchBy: "keyword", Rows: []ImportProductRow{{Row: 2, Keyword: code, Name: importPtr("Novo")}}}
	preview, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), false)
	if err != nil {
		t.Fatal(err)
	}
	req.Revision = preview.Revision
	if _, err := buscaPool.Exec(t.Context(), `UPDATE products SET stock=stock+1 WHERE store_id=$1 AND keyword=$2`, sid.String(), code); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), true); err == nil {
		t.Fatal("accepted changed stock")
	}
}

func testImportInput(t *testing.T, sid vo.StoreID, req ImportProductsRequest) ImportProductsInput {
	t.Helper()
	input, err := req.ToInput(sid.String())
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestImportProductsConcurrentRetryCreatesOnce(t *testing.T) {
	requireBuscaDB(t)
	sid := semearCatalogo(t)
	svc := NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop())
	req := ImportProductsRequest{MatchBy: "sku", Rows: []ImportProductRow{{Row: 2, SKU: importPtr("parallel-import"), Name: importPtr("Produto"), Price: importPtr(int64(100))}}}
	preview, err := svc.ImportProducts(t.Context(), testImportInput(t, sid, req), false)
	if err != nil {
		t.Fatal(err)
	}
	req.Revision = preview.Revision
	input := testImportInput(t, sid, req)
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := svc.ImportProducts(t.Context(), input, true); results <- err }()
	}
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful competing imports=%d", successes)
	}
	var count int
	if err := buscaPool.QueryRow(t.Context(), `SELECT count(*) FROM products WHERE store_id=$1 AND sku='parallel-import'`, sid.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("products=%d", count)
	}
}
