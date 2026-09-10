package erp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyInstallmentsDoNotFeedWebhookLoop(t *testing.T) {
	for _, invoiced := range []bool{false, true} {
		name := "repeated discounted payment"
		if invoiced {
			name = "invoiced order"
		}
		t.Run(name, func(t *testing.T) {
			writes := 0
			state := tinyCheckoutOrder{}
			if invoiced {
				state.InvoiceID = 123
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				writes++
				if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			provider := newTinyAgainst(t, srv)
			date := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
			payments := []providers.ERPInstallment{
				{AmountCents: 9421, DueDate: date, Note: "PAGO PIX"},
				{AmountCents: 398, DueDate: date, Note: "DESCONTO concedido (cupom/PIX) - nao cobrar"},
			}
			for range 3 {
				err := provider.SetOrderInstallments(t.Context(), "1", payments)
				if invoiced && err == nil || !invoiced && err != nil {
					t.Fatalf("unexpected result: %v", err)
				}
			}
			want := 1
			if invoiced {
				want = 0
			}
			if writes != want {
				t.Fatalf("writes=%d want=%d", writes, want)
			}
		})
	}
}

func TestTinyPaidCheckoutRequiresVerifiedCommercialSnapshot(t *testing.T) {
	for _, scenario := range []string{"incident missing freight", "customer differs", "invoice", "matching", "ignored payment write"} {
		t.Run(scenario, func(t *testing.T) {
			state := tinyCheckoutOrder{Total: 94.21, Freight: 18.59, Discount: 3.98}
			state.Customer.Name = "Demo Buyer"
			if scenario == "incident missing freight" {
				state.Total, state.Freight = 79.60, 0
			}
			if scenario == "customer differs" {
				state.Customer.Name = "instagram_handle"
			}
			if scenario == "invoice" {
				state.InvoiceID = 123
			}
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				writes++
				if scenario != "ignored payment write" {
					if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
						t.Error(err)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			provider := newTinyAgainst(t, srv)
			checkout := providers.ERPOrderCheckout{FreightCents: 1859, DiscountCents: 398,
				Customer: providers.ERPContactInput{Name: "Demo Buyer"},
				Payments: []providers.ERPInstallment{{AmountCents: 9421, DueDate: time.Now(), Note: "PAGO PIX"}},
			}
			err := provider.SyncOrderCheckout(t.Context(), "1", checkout)
			if scenario == "matching" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("incomplete checkout reported successful")
			}
			if scenario != "matching" && scenario != "ignored payment write" && writes != 0 {
				t.Fatal("divergent or invoiced order was modified")
			}
		})
	}
}
