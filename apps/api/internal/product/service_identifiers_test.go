//go:build integration

package product

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

func createCatalogProduct(t *testing.T, service *Service, storeID vo.StoreID, shipping ShippingProfileDTO) *domain.Product {
	t.Helper()
	request := CreateProductRequest{
		Name: "LUZ DECORATIVA GALHO - 2m", ExternalSource: "tiny", ExternalID: "848285025",
		Price: 7990, Stock: 7, ImageURL: "https://example.com/galho.jpg", Shipping: shipping,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	input, err := request.ToInput(storeID.String())
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func updateCatalogProduct(t *testing.T, service *Service, product *domain.Product, body string) *domain.Product {
	t.Helper()
	var request UpdateProductRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	input, err := request.ToInput(product.StoreID().String(), product.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.Update(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return view.Product
}

func TestServiceCatalogIdentifiersWithoutDimensions(t *testing.T) {
	requireBuscaDB(t)
	t.Parallel()
	storeID := semearCatalogo(t)
	service := NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop())
	created := createCatalogProduct(t, service, storeID, ShippingProfileDTO{
		SKU: " 47169001 ", Barcode: " 7893979655073\r\n",
	})
	if created.IsShippable() {
		t.Fatal("unexpected result: created.IsShippable()")
	}
	if got := created.Shipping().SKU; got != "47169001" {
		t.Fatalf("got %v, want %v", got, "47169001")
	}
	if got := created.Shipping().Barcode; got != "7893979655073" {
		t.Fatalf("got %v, want %v", got, "7893979655073")
	}
	for _, term := range []string{"47169001", "7893979655073", " 7893979655073\r\n"} {
		if got := buscar(t, storeID, term); !reflect.DeepEqual(got, []string{created.Name()}) {
			t.Fatalf("got %v, want %v", got, []string{created.Name()})
		}
	}

	// The legacy editor submits dimensions and SKU but does not know barcode.
	updated := updateCatalogProduct(t, service, created,
		`{"name":"Galho editado","price":7990,"stock":7,"active":true,"shipping":{"sku":"0047169001","packageFormat":"box"}}`)
	if got := updated.Shipping().SKU; got != "0047169001" {
		t.Fatalf("got %v, want %v", got, "0047169001")
	}
	if got := updated.Shipping().Barcode; got != "7893979655073" {
		t.Fatalf("got %v, want %v", got, "7893979655073")
	}
	if got := updated.ImageURL(); got != created.ImageURL() {
		t.Fatalf("got %v, want %v", got, created.ImageURL())
	}
	if got := buscar(t, storeID, "0047169001"); !reflect.DeepEqual(got, []string{"Galho editado"}) {
		t.Fatalf("got %v, want %v", got, []string{"Galho editado"})
	}

	// Explicit removal remains possible; omission must not be treated as removal.
	cleared := updateCatalogProduct(t, service, updated,
		`{"name":"Galho editado","price":7990,"stock":7,"active":true,"imageUrl":"","shipping":{"barcode":""}}`)
	if got := cleared.Shipping().Barcode; len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	if got := cleared.Shipping().SKU; got != "0047169001" {
		t.Fatalf("got %v, want %v", got, "0047169001")
	}
	if got := cleared.ImageURL(); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	if got := buscar(t, storeID, "7893979655073"); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestServiceTogglePreservesShippingAndImage(t *testing.T) {
	requireBuscaDB(t)
	t.Parallel()
	storeID := semearCatalogo(t)
	service := NewService(NewRepository(sqlc.New(buscaPool), buscaPool), zap.NewNop())
	dim := 10
	created := createCatalogProduct(t, service, storeID, ShippingProfileDTO{
		WeightGrams: &dim, HeightCm: &dim, WidthCm: &dim, LengthCm: &dim,
		SKU: "47169001", Barcode: "7893979655073", PackageFormat: "box",
	})
	for _, body := range []string{
		`{"name":"Galho","price":7990,"stock":7,"active":false}`,
		`{"name":"Galho","price":7990,"stock":7,"active":true,"shipping":null,"imageUrl":null}`,
	} {
		updated := updateCatalogProduct(t, service, created, body)
		if got := updated.Shipping(); !reflect.DeepEqual(got, created.Shipping()) {
			t.Fatalf("got %v, want %v", got, created.Shipping())
		}
		if got := updated.ImageURL(); got != created.ImageURL() {
			t.Fatalf("got %v, want %v", got, created.ImageURL())
		}
		if !updated.IsShippable() {
			t.Fatal("unexpected result: updated.IsShippable()")
		}
	}

	// A form can deliberately clear all dimensions without losing identifiers.
	cleared := updateCatalogProduct(t, service, created,
		`{"name":"Galho","price":7990,"stock":7,"active":true,"shipping":{"packageFormat":"box"}}`)
	if cleared.IsShippable() {
		t.Fatal("unexpected result: cleared.IsShippable()")
	}
	if got := cleared.Shipping().SKU; got != created.Shipping().SKU {
		t.Fatalf("got %v, want %v", got, created.Shipping().SKU)
	}
	if got := cleared.Shipping().Barcode; got != created.Shipping().Barcode {
		t.Fatalf("got %v, want %v", got, created.Shipping().Barcode)
	}

	otherStore := semearCatalogo(t)
	input, err := (UpdateProductRequest{Name: "Outra loja", Price: 7990}).ToInput(otherStore.String(), created.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Update(t.Context(), input)
	if !httpx.IsNotFound(err) {
		t.Fatal("unexpected result: httpx.IsNotFound(err)")
	}
}

func TestServiceSyncBackfillsLegacyIdentifiersAndPreservesStock(t *testing.T) {
	requireBuscaDB(t)
	t.Parallel()
	storeID := semearCatalogo(t)
	repo := NewRepository(sqlc.New(buscaPool), buscaPool)
	service := NewService(repo, zap.NewNop())
	created := createCatalogProduct(t, service, storeID, ShippingProfileDTO{})
	if got := buscar(t, storeID, "7893979655073"); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	adapter := NewProductSyncerAdapter(service)
	erpProduct := providers.ERPProduct{
		ID: created.ExternalID(), Name: created.Name(), Price: 7990, Stock: 50, Active: true,
		SKU: "47169001", GTIN: "7893979655073",
	}
	if err := adapter.SyncProduct(t.Context(), storeID.String(), "tiny", erpProduct, true); err != nil {
		t.Fatal(err)
	}
	synced, err := repo.GetByID(t.Context(), created.ID(), storeID)
	if err != nil {
		t.Fatal(err)
	}
	if got := synced.Stock(); got != 7 {
		t.Fatalf("got %v, want %v", got, 7)
	}
	if got := buscar(t, storeID, "7893979655073"); !reflect.DeepEqual(got, []string{created.Name()}) {
		t.Fatalf("got %v, want %v", got, []string{created.Name()})
	}
	if got := buscar(t, storeID, "47169001"); !reflect.DeepEqual(got, []string{created.Name()}) {
		t.Fatalf("got %v, want %v", got, []string{created.Name()})
	}

	// Incomplete subsequent responses must preserve identifiers already known.
	erpProduct.SKU, erpProduct.GTIN = " \t", ""
	if err := adapter.SyncProduct(t.Context(), storeID.String(), "tiny", erpProduct, true); err != nil {
		t.Fatal(err)
	}
	if got := buscar(t, storeID, "7893979655073"); !reflect.DeepEqual(got, []string{created.Name()}) {
		t.Fatalf("got %v, want %v", got, []string{created.Name()})
	}
	if got := buscar(t, storeID, "47169001"); !reflect.DeepEqual(got, []string{created.Name()}) {
		t.Fatalf("got %v, want %v", got, []string{created.Name()})
	}
}

func TestRepositoryUpdatePreservesConcurrentChanges(t *testing.T) {
	requireBuscaDB(t)
	t.Parallel()
	storeID := semearCatalogo(t)
	repo := NewRepository(sqlc.New(buscaPool), buscaPool)
	service := NewService(repo, zap.NewNop())
	stale := createCatalogProduct(t, service, storeID, ShippingProfileDTO{})
	ctx := context.Background()
	// A reservation/SYNC lands after the service read its domain snapshot.
	_, err := buscaPool.Exec(ctx, `UPDATE products SET stock=2, sku='47169001', barcode='7893979655073',
		image_url='https://example.com/new.jpg', weight_grams=100, height_cm=10, width_cm=10, length_cm=10 WHERE id=$1`, stale.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	saved, err := repo.Update(ctx, stale, updatePreservation{stock: true, shipping: true, image: true, sku: true, barcode: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Stock(); got != 2 {
		t.Fatalf("got %v, want %v", got, 2)
	}
	if got := saved.Shipping().SKU; got != "47169001" {
		t.Fatalf("got %v, want %v", got, "47169001")
	}
	if got := saved.Shipping().Barcode; got != "7893979655073" {
		t.Fatalf("got %v, want %v", got, "7893979655073")
	}
	if got := saved.ImageURL(); got != "https://example.com/new.jpg" {
		t.Fatalf("got %v, want %v", got, "https://example.com/new.jpg")
	}
	if !saved.IsShippable() {
		t.Fatal("unexpected result: saved.IsShippable()")
	}
}
