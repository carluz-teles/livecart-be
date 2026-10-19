package checkout

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestCartResponse_RecordedPaymentMethod(t *testing.T) {
	paidAt := time.Date(2026, time.October, 19, 12, 0, 0, 0, time.UTC)
	paymentID := "payment-test"
	for _, tc := range []struct {
		raw, method  string
		card         bool
		installments int
	}{
		{raw: "erp_manual", method: "erp_manual"},
		{raw: "manual", method: "manual"},
		{raw: "pix", method: "pix"},
		{raw: "boleto", method: "boleto"},
		{raw: "card", method: "card", card: true, installments: 3},
		{raw: "credit_card", method: "card", card: true, installments: 3},
		{raw: "debit_card", method: "debit_card", card: true},
		{raw: " ERP_MANUAL ", method: "erp_manual"},
		{raw: " PIX ", method: "pix"},
		{raw: " CREDIT_CARD ", method: "card", card: true, installments: 3},
		{raw: "provider-specific-method", method: "unknown"},
		{raw: ""},
		{raw: "   "},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			handler := &Handler{}
			response := handler.toCartResponse(&GetCartForCheckoutOutput{
				Cart: CartDetails{Status: "checkout", PaymentStatus: "paid", PaidAt: &paidAt, CheckoutID: &paymentID},
				// Stale card fields must not misrepresent a later manual confirmation.
				Payment: &CartPaymentInfo{
					RawMethod: tc.raw, CardBrand: "visa", LastFourDigits: "4242",
					Installments: 3, AuthorizationCode: "auth-test",
				},
			})
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Payment map[string]any `json:"payment"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if tc.method != "" {
				want = map[string]any{"method": tc.method, "paidAt": paidAt.Format(time.RFC3339), "paymentId": paymentID}
				if tc.card {
					want["cardBrand"], want["lastFourDigits"], want["authorizationCode"] = "visa", "4242", "auth-test"
				}
				if tc.installments > 0 {
					want["installments"] = float64(tc.installments)
				}
			}
			if !reflect.DeepEqual(wire.Payment, want) {
				t.Fatalf("payment JSON = %#v; want %#v", wire.Payment, want)
			}
		})
	}
}

func TestCartResponse_IncompletePaymentMetadata(t *testing.T) {
	paidAt := time.Date(2026, time.October, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		paidAt  *time.Time
		payment *CartPaymentInfo
		want    *CheckoutPaymentInfo
	}{
		{name: "no recorded payment", paidAt: &paidAt},
		{name: "no confirmation date", payment: &CartPaymentInfo{RawMethod: "erp_manual"}},
		{
			name: "manual confirmation without gateway transaction", paidAt: &paidAt,
			payment: &CartPaymentInfo{RawMethod: "erp_manual"},
			want:    &CheckoutPaymentInfo{Method: "erp_manual", PaidAt: paidAt},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &Handler{}
			response := handler.toCartResponse(&GetCartForCheckoutOutput{
				Cart:    CartDetails{Status: "checkout", PaymentStatus: "paid", PaidAt: tc.paidAt},
				Payment: tc.payment,
			})
			if !reflect.DeepEqual(response.Payment, tc.want) {
				t.Fatalf("payment = %+v; want %+v", response.Payment, tc.want)
			}
		})
	}
}
