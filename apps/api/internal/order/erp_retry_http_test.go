package order_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/httpx"
)

type erpRetryStub struct {
	err   error
	calls int
}

func (s *erpRetryStub) RetryERPFinalisation(context.Context, string, string) error {
	s.calls++
	return s.err
}

func TestERPFinalisationRetryHTTPDistinguishesConflictFromFailure(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 1000, 0, 0)
	if err := newListener(t).OnCartPaid(t.Context(), fx.cartID, fx.storeID, 1000, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		detail string
	}{
		{"commercial conflict", fmt.Errorf("finalizing: %w", &providers.TinyCheckoutReconciliationError{OrderID: "1", Status: 1, InvoiceID: 99, Fields: []string{"frete"}}), 422, "ERP_RETRY_INVALID_STATE", "frete"},
		{"merchant expense on open sale", fmt.Errorf("finalizing: %w", &providers.TinyCheckoutReconciliationError{OrderID: "2", Status: 0, Fields: []string{"despesas adicionais do pedido"}}), 422, "ERP_RETRY_INVALID_STATE", "despesas adicionais do pedido"},
		{"missing Tiny sale", fmt.Errorf("finalizing: %w", &providers.TinyCheckoutReconciliationError{OrderID: "3", Missing: true}), 422, "ERP_RETRY_INVALID_STATE", "não encontrado na conta conectada"},
		{"technical error", errors.New("database private diagnostics"), 500, "", ""},
		{"reconciled", nil, 200, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &erpRetryStub{err: tc.err}
			svc := order.NewService(order.NewRepository(testPool), zap.NewNop())
			svc.SetERPFinalisationRetrier(stub)
			app := fiber.New(fiber.Config{ErrorHandler: httpx.ErrorHandler})
			app.Use(func(c *fiber.Ctx) error { c.Locals("store_id", fx.storeID); return c.Next() })
			order.NewHandler(svc, nil).RegisterRoutes(app)
			status, body := chamar(t, app, http.MethodPost, "/orders/"+fx.cartID+"/retry-erp", "")
			if status != tc.status || stub.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", status, stub.calls, body)
			}
			if status >= 400 {
				var envelope struct {
					Code  string `json:"reason"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Code != tc.code || strings.Contains(envelope.Error, "private diagnostics") {
					t.Fatalf("incorrect error envelope: %s", body)
				}
				if tc.detail != "" && !strings.Contains(envelope.Error, tc.detail) {
					t.Fatalf("missing conflict reason: %s", body)
				}
			}
		})
	}
}
