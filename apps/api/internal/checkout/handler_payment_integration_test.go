//go:build integration

package checkout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration"
	"livecart/apps/api/lib/httpx"
)

func TestCheckoutHTTP_PaidReceiptUsesRecordedMethod(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct{ raw, method string }{
		{raw: "erp_manual", method: "erp_manual"},
		{raw: "manual", method: "manual"},
		{raw: "pix", method: "pix"},
		{raw: "boleto", method: "boleto"},
		{raw: "credit_card", method: "card"},
		{raw: "debit_card", method: "debit_card"},
		{raw: "provider-specific-method", method: "unknown"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			f := seedMerchantEdit(t)
			paidAt := time.Date(2026, time.October, 19, 12, 0, 0, 0, time.UTC)
			if _, err := testPool.Exec(t.Context(), `
				UPDATE carts SET payment_status='paid', paid_at=$2, payment_method=$3,
				checkout_id='payment-test', card_brand='visa', card_last_four='4242',
				card_installments=3, card_authorization_code='auth-test' WHERE id=$1`, f.cart, paidAt, tc.raw); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := testPool.QueryRow(t.Context(), `SELECT to_jsonb(c)::text FROM carts c WHERE id=$1`, f.cart).Scan(&before); err != nil {
				t.Fatal(err)
			}
			f.service.integrationService = integration.NewService(
				integration.NewRepository(testQueries, testPool), nil, nil, nil, nil, zap.NewNop(),
			)
			app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
			NewHandler(f.service, nil).RegisterRoutes(app)
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/public/checkout/"+f.token, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET checkout status = %d", response.StatusCode)
			}
			var wire struct {
				Data struct {
					PaymentStatus string               `json:"paymentStatus"`
					Payment       *CheckoutPaymentInfo `json:"payment"`
				} `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			p := wire.Data.Payment
			if wire.Data.PaymentStatus != "paid" || p == nil {
				t.Fatalf("missing paid receipt: %+v", wire.Data)
			}
			if p.Method != tc.method || !p.PaidAt.Equal(paidAt) || p.PaymentID != "payment-test" {
				t.Errorf("receipt misrepresents persisted payment %q: %+v", tc.raw, p)
			}
			if tc.method != "card" && tc.method != "debit_card" {
				if p.CardBrand != "" || p.LastFourDigits != "" || p.AuthorizationCode != "" || p.Installments != 0 {
					t.Errorf("non-card receipt exposes card fields: %+v", p)
				}
			}
			var after string
			if err := testPool.QueryRow(t.Context(), `SELECT to_jsonb(c)::text FROM carts c WHERE id=$1`, f.cart).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Error("reading the paid receipt mutated the cart")
			}
		})
	}
}
