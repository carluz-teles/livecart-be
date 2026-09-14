//go:build e2e

package erp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

// Explicit opt-in, with fixtures created by .tiny-demo/reproduce-checkout.py.
// Only the complete test order may receive payment updates; no stock, invoice,
// order creation or status endpoint is allowed by the transport.
func TestE2ETinyPaidCheckout(t *testing.T) {
	fixturePath := os.Getenv("TINY_E2E_CHECKOUT_FIXTURE")
	token := os.Getenv("TINY_E2E_TOKEN")
	if fixturePath == "" || token == "" {
		t.Skip("requires isolated Tiny checkout fixture and test account token")
	}
	var fixture struct {
		Run     string `json:"run"`
		Records map[string]struct {
			ID int64 `json:"id"`
		} `json:"records"`
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fixture.Run, "LC-TINY-TEST-") {
		t.Fatal("requires test-only fixture marker")
	}
	ids := make(map[string]string)
	for _, name := range []string{"reservation_order", "complete_order", "product"} {
		id := fixture.Records[name].ID
		if id <= 0 {
			t.Fatalf("missing fixture %s", name)
		}
		ids[name] = strconv.FormatInt(id, 10)
	}
	log := zap.NewNop()
	provider, err := NewTiny(TinyConfig{
		StoreID:       "tiny-isolated-checkout-test",
		IntegrationID: "tiny-isolated-checkout-test",
		Credentials:   &Credentials{AccessToken: token},
		Logger:        log,
		RateLimiter:   ratelimit.NewManager(log).GetOrCreateTiny("tiny-isolated-checkout-test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	guard := &tinyCheckoutTestTransport{
		base: http.DefaultTransport,
		allowed: map[string]bool{
			"GET /public-api/v3/info":                                true,
			"GET /public-api/v3/pedidos/" + ids["reservation_order"]: true,
			"GET /public-api/v3/pedidos/" + ids["complete_order"]:    true,
			"GET /public-api/v3/produtos/" + ids["product"]:          true,
			"GET /public-api/v3/estoque/" + ids["product"]:           true,
		},
	}
	provider.HTTPClient.Transport = guard
	provider.HTTPClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	read := func(path string, target any) {
		t.Helper()
		response, body, err := provider.DoRequest(
			t.Context(), http.MethodGet, tinyAPIBaseURL+path, nil, provider.authHeaders(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: HTTP %d", path, response.StatusCode)
		}
		if err := json.Unmarshal(body, target); err != nil {
			t.Fatal(err)
		}
	}
	var info struct {
		Company string `json:"razaoSocial"`
	}
	read("/info", &info)
	if info.Company != "ADABYTE LTDA" {
		t.Fatal("unexpected account; no writes allowed")
	}
	for _, name := range []string{"reservation_order", "complete_order"} {
		var order struct {
			Observation string `json:"observacoes"`
			InvoiceID   int64  `json:"idNotaFiscal"`
			Status      int    `json:"situacao"`
		}
		read("/pedidos/"+ids[name], &order)
		if !strings.Contains(order.Observation, fixture.Run) || order.InvoiceID != 0 || order.Status != 0 {
			t.Fatal("fixture must be an owned, open, uninvoiced test order")
		}
	}
	guard.allowed["PUT /public-api/v3/pedidos/"+ids["complete_order"]] = true
	guard.allowed["GET /public-api/v3/contas-receber?idVenda="+ids["complete_order"]+"&limit=100&offset=0"] = true
	checkout := providers.ERPOrderCheckout{
		Customer: providers.ERPContactInput{
			Name: fixture.Run + " Comprador completo", Email: "livecart-tiny-test@example.invalid", Phone: "11900000000",
		},
		FreightCents: 1859, DiscountCents: 250,
		Address: &providers.ERPShippingAddress{
			Street: "Praca da Se", Number: "42", Neighborhood: "Se", City: "Sao Paulo", State: "SP", ZipCode: "01001000",
		},
		Payments: []providers.ERPInstallment{{
			AmountCents: 6599, DueDate: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
			Method: "pix", Note: "PAGO TESTE FICTICIO " + fixture.Run,
		}},
	}
	t.Run("reservation cannot be falsely finalized", func(t *testing.T) {
		err := provider.SyncOrderCheckout(t.Context(), ids["reservation_order"], checkout)
		if err == nil || !strings.Contains(err.Error(), "frete") {
			t.Fatalf("expected missing freight reconciliation, got %v", err)
		}
		if guard.writes != 0 {
			t.Fatal("incomplete checkout caused a write")
		}
		t.Logf("reservation: %v", err)
	})
	t.Run("complete checkout and replay preserve payment", func(t *testing.T) {
		if err := provider.SyncOrderCheckout(t.Context(), ids["complete_order"], checkout); err != nil {
			t.Fatal(err)
		}
		writes := guard.writes
		if err := provider.SyncOrderCheckout(t.Context(), ids["complete_order"], checkout); err != nil {
			t.Fatal(err)
		}
		if guard.writes != writes {
			t.Fatal("replayed checkout rewrote matching installments")
		}
		t.Logf("first sync writes=%d; replay additional writes=0", writes)
	})
	t.Run("catalog uses available stock", func(t *testing.T) {
		product, err := provider.GetProduct(t.Context(), ids["product"])
		if err != nil {
			t.Fatal(err)
		}
		detail, err := provider.GetProductStockDetail(t.Context(), ids["product"])
		if err != nil {
			t.Fatal(err)
		}
		if product.SKU != fixture.Run || !product.StockKnown || product.Stock != detail.Available {
			t.Fatalf("catalog mismatch: sku=%s known=%v stock=%d detail=%+v", product.SKU, product.StockKnown, product.Stock, detail)
		}
		t.Logf("stock=%+v; catalog=%d", detail, product.Stock)
	})
}

type tinyCheckoutTestTransport struct {
	base    http.RoundTripper
	allowed map[string]bool
	mu      sync.Mutex
	next    time.Time
	writes  int
}

func (g *tinyCheckoutTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.URL.Scheme != "https" || req.URL.Host != "api.tiny.com.br" {
		return nil, fmt.Errorf("test transport rejects unexpected host")
	}
	key := req.Method + " " + req.URL.Path
	if req.URL.Path == "/public-api/v3/contas-receber" {
		key += "?" + req.URL.RawQuery
	}
	if !g.allowed[key] {
		return nil, fmt.Errorf("test transport rejects unowned endpoint %s %s", req.Method, req.URL.Path)
	}
	if wait := time.Until(g.next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	g.next = time.Now().Add(3100 * time.Millisecond)
	if req.Method != http.MethodGet {
		g.writes++
	}
	return g.base.RoundTrip(req)
}
