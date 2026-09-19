package integration

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/ratelimit"
)

type searchProvider struct {
	providers.ERPProvider
	lists   atomic.Int64
	details atomic.Int64
	fail    error
}

func (p *searchProvider) ListProducts(ctx context.Context, params providers.ListProductsParams) (*providers.ProductListResult, error) {
	p.lists.Add(1)
	if p.fail != nil {
		return nil, p.fail
	}
	products := make([]providers.ERPProduct, 20)
	for i := range products {
		products[i] = providers.ERPProduct{ID: fmt.Sprint(i), Name: "Decoracao", SKU: fmt.Sprint(i), Price: 1000, Active: true}
	}
	return &providers.ProductListResult{Products: products, HasMore: true}, nil
}
func (p *searchProvider) GetProduct(ctx context.Context, id string) (*providers.ERPProduct, error) {
	p.details.Add(1)
	return &providers.ERPProduct{ID: id, Name: "Decoracao", Stock: 3, StockKnown: true, Active: true}, nil
}
func (p *searchProvider) Name() providers.ProviderName { return providers.ProviderTiny }

type barcodeSearchProvider struct {
	searchProvider
	list func(providers.ListProductsParams) (*providers.ProductListResult, error)
}

func (p *barcodeSearchProvider) ListProducts(_ context.Context, params providers.ListProductsParams) (*providers.ProductListResult, error) {
	p.lists.Add(1)
	return p.list(params)
}

func TestTinyBarcodeSearchDoesNotWaitForUnrelatedLookups(t *testing.T) {
	p := &barcodeSearchProvider{list: func(params providers.ListProductsParams) (*providers.ProductListResult, error) {
		if params.GTIN != "7893979655073" {
			return nil, context.DeadlineExceeded
		}
		return &providers.ProductListResult{Products: []providers.ERPProduct{{ID: "galho", GTIN: params.GTIN, Active: true}}}, nil
	}}
	result, err := (&Service{logger: zap.NewNop()}).searchERPProducts(t.Context(), p, SearchProductsInput{Search: "7893979655073", SummaryOnly: true})
	if err != nil || len(result.Products) != 1 || p.lists.Load() != 1 || p.details.Load() != 0 {
		t.Fatalf("result=%+v err=%v list calls=%d detail calls=%d", result, err, p.lists.Load(), p.details.Load())
	}
}

func TestTinyBarcodeSearchFallsBackToNumericSKU(t *testing.T) {
	p := &barcodeSearchProvider{list: func(params providers.ListProductsParams) (*providers.ProductListResult, error) {
		if params.SKU != "" {
			return &providers.ProductListResult{Products: []providers.ERPProduct{{ID: "numeric-sku", SKU: params.SKU, Active: true}}}, nil
		}
		return &providers.ProductListResult{}, nil
	}}
	result, err := (&Service{logger: zap.NewNop()}).searchERPProducts(t.Context(), p, SearchProductsInput{Search: "47169001", SummaryOnly: true})
	if err != nil || len(result.Products) != 1 || result.Products[0].ID != "numeric-sku" || p.lists.Load() != 3 {
		t.Fatalf("numeric SKU fallback result=%+v err=%v calls=%d", result, err, p.lists.Load())
	}
}

func TestTinyBarcodeFailureDoesNotIssueMisleadingFallbacks(t *testing.T) {
	p := &barcodeSearchProvider{list: func(providers.ListProductsParams) (*providers.ProductListResult, error) {
		return nil, &ratelimit.ErrRateLimited{}
	}}
	_, err := (&Service{logger: zap.NewNop()}).searchERPProducts(t.Context(), p, SearchProductsInput{Search: "7893979655073", SummaryOnly: true})
	var status *httpx.ServiceError
	if !errors.As(productSearchError(err), &status) || status.Code != 503 || p.lists.Load() != 1 {
		t.Fatalf("barcode failure: %v; calls=%d", err, p.lists.Load())
	}
}

type selectedStockProvider struct {
	searchProvider
	product providers.ERPProduct
}

func (p *selectedStockProvider) GetProduct(context.Context, string) (*providers.ERPProduct, error) {
	return &p.product, nil
}

func TestTinyImportNeverTreatsUnconfirmedStockAsSoldOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		product providers.ERPProduct
		status  int
	}{
		{"unconfirmed simple", providers.ERPProduct{Active: true}, 503},
		{"known zero", providers.ERPProduct{Active: true, StockKnown: true}, 422},
		{"available simple", providers.ERPProduct{Active: true, StockKnown: true, Stock: 3}, 200},
		{"parent preview defers variant stocks", providers.ERPProduct{Active: true, IsParent: true, Variants: []providers.ERPProduct{{Active: true, StockKnown: true, Stock: 3}, {Active: true}}}, 200},
		{"parent stock irrelevant", providers.ERPProduct{Active: true, IsParent: true, Variants: []providers.ERPProduct{{Active: true, StockKnown: true, Stock: 3}}}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			product, err := readERPProductDetails(t.Context(), &selectedStockProvider{product: tc.product}, "selected")
			if tc.status == 200 {
				if err != nil || product == nil {
					t.Fatalf("product=%+v error=%v", product, err)
				}
				return
			}
			var status *httpx.ServiceError
			if !errors.As(err, &status) || status.Code != tc.status || product != nil {
				t.Fatalf("product=%+v error=%v; wanted status %d", product, err, tc.status)
			}
		})
	}
}

func TestSearchEmptySuccessCannotHideFailedLookup(t *testing.T) {
	p := &barcodeSearchProvider{list: func(params providers.ListProductsParams) (*providers.ProductListResult, error) {
		if params.SKU != "" {
			return nil, context.DeadlineExceeded
		}
		return &providers.ProductListResult{}, nil
	}}
	_, err := (&Service{logger: zap.NewNop()}).searchERPProducts(t.Context(), p, SearchProductsInput{Search: "LC-TINY", SummaryOnly: true})
	var status *httpx.ServiceError
	if !errors.As(productSearchError(err), &status) || status.Code != 503 {
		t.Fatalf("failed search was reported as absence: %v", err)
	}
}

func TestSearchQuotaWaitIsTemporaryUnavailability(t *testing.T) {
	err := productSearchError(&providers.RequestNotSentError{Err: ratelimit.ErrNaoDespachado})
	var status *httpx.ServiceError
	if !errors.As(err, &status) || status.Code != 503 {
		t.Fatalf("quota wait became an internal error: %v", err)
	}
}
func TestSearchPreviewsDoNotReadEveryProductsStock(t *testing.T) {
	svc := &Service{logger: zap.NewNop()}
	p := &searchProvider{}
	result, err := svc.searchERPProducts(t.Context(), p, SearchProductsInput{Search: "Decoracao", PageSize: 20, SummaryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Products) != 20 || p.lists.Load() != 2 || p.details.Load() != 0 {
		t.Fatalf("products=%d listing calls=%d detail calls=%d", len(result.Products), p.lists.Load(), p.details.Load())
	}
	if !result.HasMore {
		t.Fatal("truncated search hides remaining results")
	}
	for _, product := range result.Products {
		if !product.DetailsPending {
			t.Fatal("list response pretends stock was read")
		}
	}
}
func TestSearchLegacyClientStillReceivesDetails(t *testing.T) {
	svc := &Service{logger: zap.NewNop()}
	p := &searchProvider{}
	result, err := svc.searchERPProducts(t.Context(), p, SearchProductsInput{Search: "Decoracao", PageSize: 1})
	if err != nil || result.Products[0].Stock != 3 || p.details.Load() != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestSearchUnavailabilityNeverMeansNotFound(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, &ratelimit.ErrRateLimited{}} {
		svc := &Service{logger: zap.NewNop()}
		_, searchErr := svc.searchERPProducts(t.Context(), &searchProvider{fail: err}, SearchProductsInput{Search: "decoracao", SummaryOnly: true})
		var status *httpx.ServiceError
		if !errors.As(productSearchError(searchErr), &status) || status.Code != 503 {
			t.Fatalf("unavailability = %v", searchErr)
		}
	}
}

func TestSearchRequestRejectsUnboundedOrEmptyQueries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		search string
		limit  int
		valid  bool
	}{
		{"valid", "galho", 20, true}, {"missing search", "", 20, false}, {"short", "a", 20, false},
		{"zero limit", "galho", 0, false}, {"negative limit", "galho", -1, false}, {"too many results", "galho", 21, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (SearchERPProductsRequest{Search: tc.search, Limit: tc.limit}).Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	request := SearchERPProductsRequest{Search: "  ", Limit: 20}
	if _, err := request.ToInput("be0b0049-9591-4831-a4a9-66db984aa7ad", "8e66c928-59e2-4e3c-9435-9485578e4078"); err == nil {
		t.Fatal("whitespace becomes unfiltered catalog query")
	}
}
