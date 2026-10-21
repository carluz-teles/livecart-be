//go:build integration

package checkout

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

func TestCheckoutUnavailableCartContract(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct {
		name, status, cancelledReason, reason, message string
		expired                                        bool
		statusCode                                     int
	}{
		{"merged without destination", "cancelled", "merged_into_vip_cart", "CART_MERGED", "link do carrinho VIP", false, 422},
		{"merged past deadline", "cancelled", "merged_into_vip_cart", "CART_MERGED", "link do carrinho VIP", true, 422},
		{"store cancelled", "cancelled", "store_cancelled", "", "cancelado pela loja", false, 422},
		{"store cancelled past deadline", "cancelled", "store_cancelled", "", "cancelado pela loja", true, 422},
		{"blocked past deadline stays private", "cancelled", "customer_blocked", "", "não encontrado", true, 404},
		{"expired", "expired", "", "CART_EXPIRED", "expirado", false, 422},
		{"deadline elapsed", "checkout", "", "CART_EXPIRED", "expirado", true, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cartID := seedCartForPix(t)
			var deadline *time.Time
			if tc.expired {
				past := time.Now().Add(-time.Hour)
				deadline = &past
			}
			var token string
			err := testPool.QueryRow(t.Context(), `UPDATE carts
				SET status=$2,cancelled_reason=NULLIF($3,''),expires_at=$4,joined_to_cart_id=NULL
				WHERE id=$1 RETURNING token`, cartID, tc.status, tc.cancelledReason, deadline).Scan(&token)
			if err != nil {
				t.Fatal(err)
			}
			app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
			service := NewService(testRepo, testPool, nil, nil, zap.NewNop())
			NewHandler(service, nil).RegisterRoutes(app)
			response, err := app.Test(httptest.NewRequest("GET", "/api/public/checkout/"+token, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.statusCode || response.Header.Get("Location") != "" {
				t.Fatalf("status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			var message, reason string
			if err := json.Unmarshal(body["error"], &message); err != nil {
				t.Fatal(err)
			}
			if value, ok := body["reason"]; ok {
				if err := json.Unmarshal(value, &reason); err != nil {
					t.Fatal(err)
				}
			}
			if reason != tc.reason || !strings.Contains(message, tc.message) {
				t.Fatalf("reason=%q message=%q", reason, message)
			}
			// Rejected public links must not expose cart contents or infer a VIP destination.
			for key := range body {
				if key != "error" && key != "reason" {
					t.Errorf("unexpected public field %q", key)
				}
			}
			var unchanged bool
			err = testPool.QueryRow(t.Context(), `SELECT status=$2 AND payment_status='unpaid'
				AND joined_to_cart_id IS NULL FROM carts WHERE id=$1`, cartID, tc.status).Scan(&unchanged)
			if err != nil || !unchanged {
				t.Fatalf("checkout read changed cart state: %v", err)
			}
		})
	}
}
