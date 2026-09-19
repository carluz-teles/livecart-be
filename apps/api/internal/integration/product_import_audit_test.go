package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/ratelimit"
)

type auditTransport func(*http.Request) (int, string)

func (f auditTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
func auditTiny(t *testing.T, transport auditTransport) *providererp.Tiny {
	t.Helper()
	p, err := providererp.NewTiny(providererp.TinyConfig{Logger: zap.NewNop(), Credentials: &providers.Credentials{AccessToken: "audit-no-network"}})
	if err != nil {
		t.Fatal(err)
	}
	p.HTTPClient.Transport = transport
	return p
}
func auditWire(t *testing.T, err error) (int, httpx.Envelope) {
	t.Helper()
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error { return httpx.HandleServiceError(c, err) })
	res, e := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	var body httpx.Envelope
	if e = json.NewDecoder(res.Body).Decode(&body); e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, body
}

func TestAudit503MessageSurvivesHTTP(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"rate limit", productSearchError(&ratelimit.ErrRateLimited{RetryAfter: time.Minute})},
		{"unknown stock", erpImportStockUnavailable()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := auditWire(t, tc.err)
			if code != 503 || body.Reason != string(httpx.CodeErpThrottled) || body.Error == "internal server error" {
				t.Errorf("BUG: status=%d reason=%q message=%q", code, body.Reason, body.Error)
			}
		})
	}
}

func TestAuditDeletedSelectionRetainsNotFound(t *testing.T) {
	p := auditTiny(t, func(*http.Request) (int, string) { return 404, `{}` })
	_, err := readERPProductDetails(t.Context(), p, "deleted")
	code, body := auditWire(t, err)
	if code != 404 {
		t.Errorf("BUG: Tiny 404 became HTTP %d %q", code, body.Error)
	}
}

func TestAuditLongNumericSKUStillSearchable(t *testing.T) {
	var skuCalls int
	p := auditTiny(t, func(r *http.Request) (int, string) {
		if r.URL.Query().Get("gtin") != "" {
			return 400, `{"message":"invalid gtin integer"}`
		}
		if r.URL.Query().Get("codigo") != "" {
			skuCalls++
			return 200, `{"itens":[{"id":123,"sku":"1234567890123456789012345","descricao":"Numeric SKU","situacao":"A"}],"paginacao":{"limit":20,"total":1}}`
		}
		return 200, `{"itens":[],"paginacao":{"limit":20,"total":0}}`
	})
	out, err := (&Service{logger: zap.NewNop()}).searchERPProducts(t.Context(), p, SearchProductsInput{Search: "1234567890123456789012345", SummaryOnly: true})
	if err != nil || skuCalls == 0 || len(out.Products) != 1 {
		t.Errorf("BUG: valid numeric SKU blocked by barcode heuristic; sku calls=%d error=%v", skuCalls, err)
	}
}

func TestAuditAvailableStockAndRequestEncoding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stockBody  string
		wantStatus int
		wantStock  int
	}{
		{"reserved units excluded", `{"saldo":10,"reservado":7,"disponivel":3}`, 200, 3},
		{"known zero", `{"saldo":10,"reservado":10,"disponivel":0}`, 422, 0},
		{"physical only unknown", `{"saldo":10}`, 503, 0},
		{"malformed stock", `not json`, 503, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := auditTiny(t, func(r *http.Request) (int, string) {
				if strings.Contains(r.URL.Path, "/estoque/") {
					return 200, tc.stockBody
				}
				return 200, `{"id":123,"descricao":"Vela","situacao":"A","estoque":{"quantidade":10},"precos":{"preco":10}}`
			})
			product, err := readERPProductDetails(t.Context(), p, "123")
			status := 200
			if err != nil {
				status = httpx.StatusFromError(err)
			}
			if status != tc.wantStatus || (product != nil && product.Stock != tc.wantStock) {
				t.Fatalf("product=%+v status=%d err=%v", product, status, err)
			}
		})
	}
	for _, term := range []string{"Coração & Vela / 2", "0001234567890"} {
		p := auditTiny(t, func(r *http.Request) (int, string) {
			q := r.URL.Query()
			if q.Get("nome") != term {
				t.Errorf("term changed: %q", q.Get("nome"))
			}
			return 200, `{"itens":[],"paginacao":{"limit":20,"total":0}}`
		})
		if _, err := p.ListProducts(t.Context(), providers.ListProductsParams{Search: term, PageSize: 20}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditLargeVariantSelectionWithinBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var variants []map[string]any
		for i := 1; i <= 22; i++ {
			variants = append(variants, map[string]any{"id": i, "sku": fmt.Sprint(i), "descricao": "Variant", "precos": map[string]any{"preco": 10}})
		}
		body, _ := json.Marshal(map[string]any{"id": 100, "descricao": "Many variants", "situacao": "A", "tipo": "V", "variacoes": variants})
		calls := 0
		p := auditTiny(t, func(r *http.Request) (int, string) {
			calls++
			if strings.Contains(r.URL.Path, "/estoque/") {
				return 200, `{"disponivel":3}`
			}
			return 200, string(body)
		})
		p.RateLimiter = ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny("isolated-audit")
		ctx, cancel := context.WithTimeout(t.Context(), erpProductDetailsTimeout)
		defer cancel()
		started := time.Now()
		selected, err := readERPProductDetails(ctx, p, "100")
		if err != nil || calls != 1 || selected == nil || len(selected.Variants) != 22 {
			t.Fatalf("cannot choose variants without reading all stock; sent=%d elapsed=%s error=%v", calls, time.Since(started), err)
		}
		for _, variant := range selected.Variants {
			if variant.StockKnown || variant.Stock != 0 {
				t.Fatalf("preview claims an unconfirmed stock: %+v", variant)
			}
		}
	})
}

func TestAuditVariantReReadCannotTurnUnknownStockIntoZero(t *testing.T) {
	parent := &providers.ERPProduct{IsParent: true, Variants: []providers.ERPProduct{{ID: "1", Active: true, StockKnown: true, Stock: 3}}}
	p := &selectedStockProvider{product: providers.ERPProduct{ID: "1", Active: true, StockKnown: false}}
	(&Service{logger: zap.NewNop()}).enrichVariantsFromIndividualGets(t.Context(), p, parent)
	got := parent.Variants[0]
	if got.Stock == 0 && got.StockKnown {
		t.Errorf("BUG: unknown second reading became known zero: %+v", got)
	}
}
