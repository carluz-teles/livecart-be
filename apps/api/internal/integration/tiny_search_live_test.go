//go:build tiny_search_e2e

package integration

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/lib/ratelimit"
)

type searchCountingTransport struct{ calls atomic.Int64 }

func (c *searchCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		panic("search smoke must remain read-only")
	}
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(req)
}

func TestTinyDemoSearchListsBeforeLoadingSelectedStock(t *testing.T) {
	token := os.Getenv("TINY_E2E_TOKEN")
	if token == "" {
		t.Skip("requires explicit Tiny demo token")
	}
	provider, err := providererp.NewTiny(providererp.TinyConfig{IntegrationID: "demo-search", StoreID: "demo-search", Credentials: &providers.Credentials{AccessToken: token}, Logger: zap.NewNop(), RateLimiter: ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny("demo-search")})
	if err != nil {
		t.Fatal(err)
	}
	transport := &searchCountingTransport{}
	provider.HTTPClient.Transport = transport
	provider.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	info, err := provider.TestConnection(t.Context())
	if err != nil || !info.Success || info.AccountInfo["empresa"] != "ADABYTE LTDA" {
		t.Fatalf("unexpected sandbox identity: %v", err)
	}
	svc := &Service{logger: zap.NewNop()}
	before := transport.calls.Load()
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), erpSearchTimeout)
	defer cancel()
	results, err := svc.searchERPProducts(ctx, provider, SearchProductsInput{Search: "LC-TINY", PageSize: 20, SummaryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	calls := transport.calls.Load() - before
	if calls != 2 || len(results.Products) == 0 {
		t.Fatalf("calls=%d products=%d", calls, len(results.Products))
	}
	t.Logf("Live Tiny search: %d previews, %d HTTP GETs, %s", len(results.Products), calls, time.Since(start).Round(time.Millisecond))
	before = transport.calls.Load()
	start = time.Now()
	selected, err := provider.GetProduct(t.Context(), "371893889")
	if err != nil {
		t.Fatal(err)
	}
	if !selected.StockKnown {
		t.Fatal("selected stock not verified")
	}
	t.Logf("Selected marked demo product: available=%d, GETs=%d, elapsed=%s", selected.Stock, transport.calls.Load()-before, time.Since(start).Round(time.Millisecond))
	if !isGTIN(selected.GTIN) {
		t.Fatal("marked test product needs a GTIN to validate barcode lookup")
	}
	before = transport.calls.Load()
	start = time.Now()
	barcodeCtx, barcodeCancel := context.WithTimeout(t.Context(), erpSearchTimeout)
	defer barcodeCancel()
	byBarcode, err := svc.searchERPProducts(barcodeCtx, provider, SearchProductsInput{Search: selected.GTIN, SummaryOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load()-before != 1 || len(byBarcode.Products) == 0 || byBarcode.Products[0].ID != selected.ID {
		t.Fatalf("barcode results=%+v calls=%d", byBarcode, transport.calls.Load()-before)
	}
	t.Logf("Live barcode with quota limiter: %d GET, %s", transport.calls.Load()-before, time.Since(start).Round(time.Millisecond))
}
