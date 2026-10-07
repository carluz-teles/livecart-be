package erp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyInvoiceRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		notFound bool
	}{
		{"not found", http.StatusNotFound, `{}`, true},
		{"no emitted invoice", http.StatusOK, `{"id":0}`, true},
		{"provider error", http.StatusBadRequest, `{"mensagem":"invalid request"}`, false},
		{"malformed response", http.StatusOK, `{`, false},
		{"invalid number", http.StatusOK, `{"id":321,"numero":true}`, false},
		{"invalid series", http.StatusOK, `{"id":321,"serie":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			invoice, err := newTinyAgainst(t, srv).GetInvoiceByID(t.Context(), "321")
			if err == nil || invoice != nil || errors.Is(err, providers.ErrInvoiceNotFound) != tc.notFound {
				t.Fatalf("invalid invoice accepted/misclassified: %+v %v", invoice, err)
			}
		})
	}
}

func TestTinyInvoicePreservesDatesAndStatuses(t *testing.T) {
	for _, value := range []string{"2026-10-06", "2026-10-06 00:00:00", "2026-10-06T00:00:00-03:00", "06/10/2026", "06/10/2026 00:00:00"} {
		invoice := tinyNotaFiscalToERP(tinyNotaFiscalLite{ID: 321, DataEmissao: value})
		if !invoice.IssuedAt.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, tinyLocation)) {
			t.Fatalf("date %q parsed as %v", value, invoice.IssuedAt)
		}
	}
	if !tinyNotaFiscalToERP(tinyNotaFiscalLite{DataEmissao: "invalid"}).IssuedAt.IsZero() {
		t.Fatal("invalid issue date fabricated")
	}
	for _, tc := range []struct {
		status json.Number
		want   providers.ERPInvoiceStatus
	}{
		{"1", providers.ERPInvoiceStatusPending},
		{"3", providers.ERPInvoiceStatusCancelled},
		{"5", providers.ERPInvoiceStatusRejected},
		{"6", providers.ERPInvoiceStatusAuthorized},
		{"7", providers.ERPInvoiceStatusAuthorized},
		{"10", providers.ERPInvoiceStatusRejected},
	} {
		invoice := tinyNotaFiscalToERP(tinyNotaFiscalLite{ID: 321, Situacao: tc.status})
		if invoice.Status != tc.want {
			t.Fatalf("status %s mapped to %s", tc.status, invoice.Status)
		}
	}
}

func TestTinyInvoiceContract(t *testing.T) {
	for _, tc := range []struct {
		name, payload, number, series string
	}{
		{"string identifiers", `{"id":321,"numero":"021490","serie":"01","situacao":"7","dataEmissao":"2026-10-06"}`, "021490", "01"},
		{"numeric identifiers", `{"id":321,"numero":21490,"serie":1,"situacao":7,"dataEmissao":"2026-10-06"}`, "21490", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/notas/321" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			invoice, err := newTinyAgainst(t, srv).GetInvoiceByID(t.Context(), "321")
			if err != nil {
				t.Fatal(err)
			}
			wantDate := time.Date(2026, 10, 6, 0, 0, 0, 0, tinyLocation)
			if invoice.InvoiceID != "321" || invoice.Number != tc.number || invoice.Series != tc.series ||
				invoice.Status != providers.ERPInvoiceStatusAuthorized || !invoice.IssuedAt.Equal(wantDate) {
				t.Fatalf("invoice contract lost data: %+v", invoice)
			}
		})
	}
}

func TestTinyInvoiceXMLContract(t *testing.T) {
	for _, payload := range []string{`{"xmlNfe":"<nfe>test</nfe>"}`, `<nfe>test</nfe>`} {
		t.Run(payload, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/notas/321/xml" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(payload))
			}))
			defer srv.Close()
			xml, err := newTinyAgainst(t, srv).GetInvoiceXML(t.Context(), "321")
			if err != nil || string(xml) != "<nfe>test</nfe>" {
				t.Fatalf("xml=%q err=%v", xml, err)
			}
		})
	}
}

func TestTinyInvoiceOrderShapes(t *testing.T) {
	for _, tc := range []struct {
		name, payload, wantID string
	}{
		{"embedded", `{"idNotaFiscal":321,"notaFiscal":{"id":321,"numero":"021490","situacao":7}}`, "321"},
		{"id only", `{"idNotaFiscal":321}`, "321"},
		{"history", `{"notasFiscais":[{"id":322,"situacao":3},{"id":321,"situacao":7}]}`, "321"},
		{"ecommerce", `{"ecommerce":{"notaFiscal":{"id":321,"situacao":7}}}`, "321"},
		{"missing", `{"notaFiscal":{"id":0}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				switch r.URL.Path {
				case "/pedidos/123":
					_, _ = w.Write([]byte(tc.payload))
				case "/notas/321":
					_, _ = w.Write([]byte(`{"id":321,"numero":"021490","situacao":7}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			invoice, err := newTinyAgainst(t, srv).GetInvoiceByOrder(t.Context(), "123")
			if tc.wantID == "" {
				if !errors.Is(err, providers.ErrInvoiceNotFound) {
					t.Fatalf("missing invoice: %+v %v", invoice, err)
				}
				return
			}
			if err != nil || invoice.InvoiceID != tc.wantID {
				t.Fatalf("invoice=%+v err=%v", invoice, err)
			}
			wantReads := 1
			if tc.name == "id only" {
				wantReads = 2
			}
			if reads != wantReads {
				t.Fatalf("reads=%d want=%d", reads, wantReads)
			}
		})
	}
}
