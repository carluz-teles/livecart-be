package httpx

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestERPTemporaryErrorNeverLeaksProviderMessages(t *testing.T) {
	for _, category := range []Category{CategoryDomain, CategoryInfrastructure, CategoryRepository} {
		t.Run(string(category), func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(c *fiber.Ctx) error {
				return HandleServiceError(c, &ServiceError{Code: 503, Category: category, Reason: string(CodeErpThrottled), Message: "private provider response"})
			})
			res, err := app.Test(httptest.NewRequest("GET", "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var body Envelope
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error == "private provider response" {
				t.Fatal("provider message leaked")
			}
			if category == CategoryDomain {
				if body.Reason != string(CodeErpThrottled) || res.Header.Get("Retry-After") != "5" {
					t.Fatalf("lost safe temporary error: %+v", body)
				}
			} else if body.Reason != string(CodeInternal) || body.Error != "internal server error" {
				t.Fatalf("infrastructure error exposed: %+v", body)
			}
		})
	}
}
