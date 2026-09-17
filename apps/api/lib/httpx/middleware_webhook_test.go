package httpx

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

type webhookSlugProbe struct{ t *testing.T }

func (p webhookSlugProbe) GetSlugByID(ctx context.Context, id string) (string, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 500*time.Millisecond {
		p.t.Error("unbounded database lookup before webhook handler")
	}
	return "", errors.New("database unavailable")
}
func TestWebhookStoreContextFailureDoesNotBlockReceiver(t *testing.T) {
	app := fiber.New()
	called := false
	app.Post("/:storeId", WebhookStoreContext(webhookSlugProbe{t: t}), func(c *fiber.Ctx) error {
		called = true
		if GetStoreID(c) != "test-store" {
			t.Error("lost store identity")
		}
		return c.SendStatus(200)
	})
	res, err := app.Test(httptest.NewRequest("POST", "/test-store", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if !called || res.StatusCode != 200 {
		t.Fatalf("receiver called=%v status=%d", called, res.StatusCode)
	}
}
