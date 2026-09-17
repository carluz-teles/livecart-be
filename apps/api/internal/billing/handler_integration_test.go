//go:build integration

package billing

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestSubscriptionResponsePreservesBillingInterval(t *testing.T) {
	for _, interval := range []string{"monthly", "semestral", "annual"} {
		t.Run(interval, func(t *testing.T) {
			svc := newTestService(t)
			storeID := seedStore(t)
			seedSubscription(t, storeID, StatusActive)
			_, err := testPool.Exec(t.Context(),
				`UPDATE subscriptions SET billing_interval=$2 WHERE store_id=$1`, storeID, interval)
			if err != nil {
				t.Fatal(err)
			}

			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error {
				c.Locals("store_id", storeID)
				return c.Next()
			})
			NewHandler(svc).RegisterRoutes(app)
			response, err := app.Test(httptest.NewRequest("GET", "/billing/subscription", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if body.Data["billingInterval"] != interval {
				t.Fatalf("billingInterval = %v, want %s", body.Data["billingInterval"], interval)
			}
			if body.Data["status"] != StatusActive || body.Data["blocked"] != false {
				t.Fatalf("active subscription lost access: %v", body.Data)
			}

			// /users/sync and the billing endpoint must carry the same contract.
			state, err := svc.GetState(t.Context(), storeID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot["billingInterval"] != interval {
				t.Fatalf("shared snapshot interval = %v, want %s", snapshot["billingInterval"], interval)
			}
		})
	}
}
