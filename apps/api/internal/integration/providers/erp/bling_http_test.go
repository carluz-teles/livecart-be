package erp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

func TestBlingOAuthErrorClassifiesWithoutLeakingResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"revoked", 400, `{"error":"invalid_grant","error_description":"secret-token"}`, true},
		{"invalid app", 401, `{"error":{"type":"invalid_client"}}`, true},
		{"quota", 429, `{"error":{"type":"TOO_MANY_REQUESTS"}}`, false},
		{"server", 503, `{"error":"invalid_grant"}`, false},
		{"unknown", 400, `{"error":"secret-token"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := novoBlingOAuthErro(tc.status, []byte(tc.body))
			var classified interface{ Permanent() bool }
			if !errors.As(err, &classified) || classified.Permanent() != tc.permanent {
				t.Fatalf("classification: %v", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("token response leaked")
			}
		})
	}
}

func TestBlingDailyQuotaBlocksOtherRequestsWithoutInlineRetry(t *testing.T) {
	calls := 0
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"type":"TOO_MANY_REQUESTS","period":"day","limit":120000}}`)
	})
	b.RateLimiter = ratelimit.NewManager(zap.NewNop()).GetOrCreateBling("account", 2)
	_, err := b.ListProducts(context.Background(), providers.ListProductsParams{})
	if status, ok := StatusDoErroBling(err); !ok || status != 429 {
		t.Fatalf("HTTP cause lost: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = b.ListProducts(ctx, providers.ListProductsParams{})
	if !errors.Is(err, ratelimit.ErrNaoDespachado) || calls != 1 {
		t.Fatalf("daily quota retried: calls=%d error=%v", calls, err)
	}
}

func TestBlingCreateOrderDoesNotWriteWhenMarkerLookupFails(t *testing.T) {
	for _, status := range []int{401, 403, 500, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			writes := 0
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"period":"day"}}`)
			})
			_, err := b.CreateOrder(context.Background(), providers.ERPOrder{ExternalID: "cart", ContactID: "1"})
			if err == nil || writes != 0 {
				t.Fatalf("unsafe create: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestBlingMarkerRejectsDuplicateOrMissingIDs(t *testing.T) {
	for _, body := range []string{`{"data":[{"id":1,"numeroLoja":"lc-cart-test"},{"id":2,"numeroLoja":"lc-cart-test"}]}`, `{"data":[{"id":0,"numeroLoja":"lc-cart-test"}]}`} {
		t.Run(body, func(t *testing.T) {
			b, _ := bancadaBling(t, respJSON(body))
			if _, err := b.FindOrderIDByMarker(context.Background(), "lc-cart-test"); err == nil {
				t.Fatal("unsafe marker accepted")
			}
		})
	}
}

func TestBlingStockBatchBoundsRequestsAndStopsAfterAllFound(t *testing.T) {
	calls := 0
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		ids := r.URL.Query()["idsProdutos[]"]
		if len(ids) > 100 {
			t.Errorf("unbounded request: %d", len(ids))
		}
		if r.URL.Query().Get("filtroSaldoEstoque") != "1" {
			t.Error("unneeded balance filter")
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			var n int
			fmt.Sscan(id, &n)
			data = append(data, map[string]any{"produto": map[string]int{"id": n}, "saldoFisicoTotal": 10, "saldoVirtualTotal": 4})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	ids := make([]string, 0, 206)
	for i := 1; i <= 205; i++ {
		ids = append(ids, fmt.Sprint(i))
	}
	ids = append(ids, "1")
	got, err := b.GetProductStockBatch(context.Background(), ids)
	if err != nil || len(got) != 205 || calls != 3 {
		t.Fatalf("batch: calls=%d products=%d err=%v", calls, len(got), err)
	}
	if got["1"].Available != 4 || got["1"].Reserved != 6 {
		t.Fatalf("available stock lost: %+v", got["1"])
	}
}

func TestBlingProductMissingStockIsUnknown(t *testing.T) {
	b, _ := bancadaBling(t, respJSON(`{"data":{"id":1,"nome":"produto","situacao":"A"}}`))
	p, err := b.GetProduct(context.Background(), "1")
	if err != nil || p.StockKnown {
		t.Fatalf("missing stock converted to zero: %+v %v", p, err)
	}
}

func TestBlingVariantFailurePropagatesForRetry(t *testing.T) {
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "variacoes") {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"data":{"id":1,"formato":"V"}}`)
	})
	if _, err := b.GetProduct(context.Background(), "1"); err == nil {
		t.Fatal("partial variant import reported success")
	}
}

func TestBlingShippingDimensionUnitsFollowSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit int
		size float64
		cm   int
	}{
		{"meters", 0, .25, 25}, {"centimeters", 1, 25, 25}, {"millimeters", 2, 250, 25}, {"fractional mm", 2, 251, 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p blingProduto
			p.PesoBruto = 1
			p.Dimensoes.Altura = tc.size
			p.Dimensoes.Largura = tc.size
			p.Dimensoes.Profundidade = tc.size
			p.Dimensoes.UnidadeMedida = tc.unit
			got, _ := blingFrete(p)
			if got == nil || got.HeightCm != tc.cm {
				t.Fatalf("dimension conversion: %+v", got)
			}
		})
	}
}

func TestBlingContactUpdatePreservesRequiredAndMerchantFields(t *testing.T) {
	var sent map[string]any
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":{"id":1,"nome":"Anterior","tipo":"F","situacao":"A","emailNotaFiscal":"fiscal@example.test","financeiro":{"limiteCredito":900}}}`)
			return
		}
		json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(204)
	})
	err := b.UpdateContact(context.Background(), "1", providers.ERPContactInput{Name: "Atual", Email: "atual@example.test"})
	if err != nil || sent["tipo"] != "F" || sent["situacao"] != "A" || sent["nome"] != "Atual" || sent["emailNotaFiscal"] != "fiscal@example.test" || sent["financeiro"] == nil {
		t.Fatalf("contact overwritten or incomplete: %+v %v", sent, err)
	}
}

func TestBlingPaymentMethodPaginationAndDefaultPriority(t *testing.T) {
	pages := 0
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		data := []map[string]any{}
		if r.URL.Query().Get("pagina") == "1" {
			for i := 0; i < 100; i++ {
				data = append(data, map[string]any{"id": i + 1, "situacao": 0})
			}
		} else {
			data = append(data, map[string]any{"id": 200, "situacao": 1, "padrao": 1, "tipoPagamento": 17}, map[string]any{"id": 101, "situacao": 1, "padrao": 0, "tipoPagamento": 17})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	id, err := b.formaPagamentoPara(context.Background(), "pix")
	if err != nil || id != 200 || pages != 2 {
		t.Fatalf("payment default lost: id=%d pages=%d err=%v", id, pages, err)
	}
}

func TestBlingFinancialWritesPreserveMerchantInstallments(t *testing.T) {
	for _, operation := range []string{"items", "installments"} {
		t.Run(operation, func(t *testing.T) {
			writes := 0
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if strings.Contains(r.URL.Path, "/formas-pagamentos") {
					respJSON(formasDaContaReal)(w, r)
					return
				}
				respJSON(`{"data":{"id":1,"itens":[],"parcelas":[{"valor":10,"observacoes":"Combinado com cliente","formaPagamento":{"id":11010299}}]}}`)(w, r)
			})
			var err error
			if operation == "items" {
				err = b.UpdateOrderItems(context.Background(), "1", []providers.ERPOrderItem{{ProductID: "10", Quantity: 1, UnitPrice: 10}})
			} else {
				err = b.SetOrderInstallments(context.Background(), "1", []providers.ERPInstallment{{AmountCents: 1000, DueDate: time.Now(), Method: "pix"}})
			}
			if err == nil || writes != 0 {
				t.Fatalf("merchant schedule overwritten: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestBlingInstallmentsReadbackDetectsChangedMethodWithSameTotal(t *testing.T) {
	var saved map[string]any
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			respJSON(formasDaContaReal)(w, r)
			return
		}
		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Error(err)
			}
			respJSON(`{"data":{"id":1}}`)(w, r)
			return
		}
		if saved == nil {
			respJSON(`{"data":{"id":1,"itens":[],"parcelas":[]}}`)(w, r)
			return
		}
		parcel := saved["parcelas"].([]any)[0].(map[string]any)
		parcel["formaPagamento"] = map[string]any{"id": 11010299}
		json.NewEncoder(w).Encode(map[string]any{"data": saved})
	})
	err := b.SetOrderInstallments(context.Background(), "1", []providers.ERPInstallment{{AmountCents: 1000, DueDate: time.Now(), Method: "pix", Note: "PAGO — pix"}})
	if err == nil || !strings.Contains(err.Error(), "REESCREVEU") {
		t.Fatalf("changed payment method accepted: %v", err)
	}
}

func TestBlingAddingItemPreservesPaidAmountsBeforeLedgerReconciliation(t *testing.T) {
	var written map[string]any
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/formas-pagamentos") {
			respJSON(formasDaContaReal)(w, r)
			return
		}
		if r.Method == http.MethodPut {
			json.NewDecoder(r.Body).Decode(&written)
			respJSON(`{"data":{"id":1}}`)(w, r)
			return
		}
		respJSON(`{"data":{"id":1,"itens":[{"produto":{"id":10},"quantidade":1,"valor":10}],"parcelas":[{"valor":10,"observacoes":"PAGO LiveCart — pix first-charge","formaPagamento":{"id":11010305},"dataVencimento":"2026-09-01"}]}}`)(w, r)
	})
	if err := b.UpdateOrderItems(context.Background(), "1", []providers.ERPOrderItem{{ProductID: "10", Quantity: 2, UnitPrice: 1000}}); err != nil {
		t.Fatal(err)
	}
	rows := written["parcelas"].([]any)
	paid := rows[0].(map[string]any)
	due := rows[1].(map[string]any)
	if blingMoney(paid["valor"]) != 1000 || blingMoney(due["valor"]) != 1000 || !strings.HasPrefix(due["observacoes"].(string), "A PAGAR") {
		t.Fatalf("extra item increased received amount: %+v", rows)
	}
}
