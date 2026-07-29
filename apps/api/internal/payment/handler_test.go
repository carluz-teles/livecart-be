package payment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// fakePagarmeAdmin is a stub PagarmeAdminService that records the last input it
// received and returns preconfigured results, so the handler's parsing,
// validation and wiring can be exercised without the integration service.
type fakePagarmeAdmin struct {
	lastConnectInput ConnectPagarmeInput
	connectResp      any
	connectErr       error

	statusResp *PagarmeWebhookStatusResponse
	statusErr  error
}

func (f *fakePagarmeAdmin) ConnectPagarme(_ context.Context, in ConnectPagarmeInput) (any, error) {
	f.lastConnectInput = in
	return f.connectResp, f.connectErr
}

func (f *fakePagarmeAdmin) GetPagarmeWebhookStatus(_ context.Context, _, _ string) (*PagarmeWebhookStatusResponse, error) {
	return f.statusResp, f.statusErr
}

func (f *fakePagarmeAdmin) TestPagarmeWebhook(_ context.Context, _, _ string) (*PagarmeWebhookTestResponse, error) {
	return &PagarmeWebhookTestResponse{URL: "https://example.test", Reachable: true}, nil
}

func (f *fakePagarmeAdmin) RunPagarmeWebhookLiveTest(_ context.Context, _, _ string) (*PagarmeWebhookLiveTestResponse, error) {
	return &PagarmeWebhookLiveTestResponse{OrderCode: "or_test", Delivered: true}, nil
}

// newTestApp builds a Fiber app with the payment routes mounted under a
// store-scoped group whose middleware seeds store_id, mirroring production
// wiring closely enough to drive the handler end to end.
func newTestApp(admin PagarmeAdminService) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
	scoped := app.Group("/stores/:storeId", func(c *fiber.Ctx) error {
		c.Locals("store_id", c.Params("storeId"))
		return c.Next()
	})
	NewHandler(admin).RegisterRoutes(scoped)
	return app
}

func TestHandler_ConnectPagarme(t *testing.T) {
	const validBody = `{"secretKey":"sk_test_abc123","publicKey":"pk_test_abc123"}`

	tests := []struct {
		name       string
		body       string
		connectErr error
		wantStatus int
	}{
		{
			name:       "malformed json is rejected as bad request",
			body:       `{`,
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing required keys fails validation",
			body:       `{}`,
			wantStatus: fiber.StatusUnprocessableEntity,
		},
		{
			name:       "keys shorter than the minimum fail validation",
			body:       `{"secretKey":"sk_1","publicKey":"pk_1"}`,
			wantStatus: fiber.StatusUnprocessableEntity,
		},
		{
			name:       "valid body reaches the admin and returns ok",
			body:       validBody,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "service error is mapped to its status",
			body:       validBody,
			connectErr: httpx.ErrUnprocessable("invalid pagarme keys"),
			wantStatus: fiber.StatusUnprocessableEntity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admin := &fakePagarmeAdmin{
				connectResp: map[string]string{"id": "int_123"},
				connectErr:  tt.connectErr,
			}
			app := newTestApp(admin)

			req := httptest.NewRequest(http.MethodPost,
				"/stores/store_1/integrations/payment/pagarme/connect",
				strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

// TestHandler_ConnectPagarme_ToInput asserts the request maps to the service
// input verbatim (including the scoped store id from Locals).
func TestHandler_ConnectPagarme_ToInput(t *testing.T) {
	admin := &fakePagarmeAdmin{connectResp: map[string]string{"id": "int_123"}}
	app := newTestApp(admin)

	body := `{"secretKey":"sk_test_abc123","publicKey":"pk_test_abc123","webhookUsername":"u","webhookPassword":"p"}`
	req := httptest.NewRequest(http.MethodPost,
		"/stores/store_42/integrations/payment/pagarme/connect",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := admin.lastConnectInput
	want := ConnectPagarmeInput{
		StoreID:         "store_42",
		SecretKey:       "sk_test_abc123",
		PublicKey:       "pk_test_abc123",
		WebhookUsername: "u",
		WebhookPassword: "p",
	}
	if got != want {
		t.Fatalf("ConnectPagarmeInput = %+v, want %+v", got, want)
	}
}

func TestHandler_GetPagarmeWebhookStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusResp *PagarmeWebhookStatusResponse
		statusErr  error
		wantStatus int
	}{
		{
			name:       "ok passes the response through",
			statusResp: &PagarmeWebhookStatusResponse{ExpectedURL: "https://hook.test", Configured: true},
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "not found is mapped to 404",
			statusErr:  httpx.ErrNotFound("integration not found"),
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admin := &fakePagarmeAdmin{statusResp: tt.statusResp, statusErr: tt.statusErr}
			app := newTestApp(admin)

			req := httptest.NewRequest(http.MethodGet,
				"/stores/store_1/integrations/int_1/pagarme/webhook-status", nil)

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus != fiber.StatusOK {
				return
			}

			var env struct {
				Data PagarmeWebhookStatusResponse `json:"data"`
			}
			raw, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal body %q: %v", raw, err)
			}
			if env.Data.ExpectedURL != tt.statusResp.ExpectedURL {
				t.Fatalf("expectedUrl = %q, want %q", env.Data.ExpectedURL, tt.statusResp.ExpectedURL)
			}
		})
	}
}
