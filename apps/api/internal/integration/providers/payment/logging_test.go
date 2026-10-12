package payment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mercadopago/sdk-go/pkg/mperror"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func sensitiveCardInput() CardPaymentInput {
	return CardPaymentInput{
		CartID: "cart-safe", Token: "card-token-secret", TotalAmount: 29025, Installments: 3,
		IPAddress: "192.0.2.42", DeviceID: "device-secret",
		Customer: CheckoutCustomer{
			Name: "Private Buyer", Email: "private@example.test", Document: "12345678900", Phone: "11912345678",
			Address: &CheckoutAddress{Street: "Private Street", Number: "42", ZipCode: "01234567"},
		},
		Metadata: map[string]any{"cart_token": "checkout-token-secret"},
	}
}

func assertNoPaymentSecrets(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{
			"card-token-secret", "checkout-token-secret", "Private Buyer", "private@example.test",
			"12345678900", "11912345678", "Private Street", "device-secret", "192.0.2.42",
			"4111111111111111", "request_payload", "response_body", "gateway_response", "antifraud_response",
		} {
			if strings.Contains(string(encoded), secret) || strings.Contains(entry.Message, secret) {
				t.Errorf("%s logged sensitive value %q", entry.Message, secret)
			}
		}
	}
}

func pagarmeLogFixture(t *testing.T, log *zap.Logger, respond func(*http.Request) (int, string)) *Pagarme {
	t.Helper()
	p, err := NewPagarme(PagarmeConfig{Credentials: &Credentials{APIKey: "test"}, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	p.HTTPClient = &http.Client{Transport: statusTransport(func(req *http.Request) (*http.Response, error) {
		status, body := respond(req)
		return &http.Response{
			StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return p
}

func TestPagarmeCardOutcomeLogsOnlySafeFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status, transaction, message string
		want                               PaymentState
	}{
		{name: "declined", status: "failed", transaction: "not_authorized", want: PaymentRejected, message: "rejected"},
		{name: "approved", status: "paid", transaction: "captured", want: PaymentApproved, message: "captured"},
		{name: "pending", status: "pending", transaction: "pending", want: PaymentPending, message: "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.InfoLevel)
			input := sensitiveCardInput()
			var order map[string]any
			calls := 0
			p := pagarmeLogFixture(t, zap.New(core), func(req *http.Request) (int, string) {
				calls++
				switch req.URL.Path {
				case "/core/v5/customers":
					return 200, `{"id":"cus_safe"}`
				case "/core/v5/customers/cus_safe/cards":
					var card map[string]any
					if err := json.NewDecoder(req.Body).Decode(&card); err != nil {
						t.Fatal(err)
					}
					if card["token"] != input.Token {
						t.Fatalf("card token sent to provider was altered: %v", card)
					}
					return 200, `{"id":"card_safe"}`
				case "/core/v5/orders":
					if err := json.NewDecoder(req.Body).Decode(&order); err != nil {
						t.Fatal(err)
					}
					paidAt := ""
					if tc.want == PaymentApproved {
						paidAt = "2026-10-09T18:07:24Z"
					}
					return 200, fmt.Sprintf(`{
						"id":"or_safe","code":"cart-safe","status":%q,"amount":29025,
						"customer":{"name":"Private Buyer","email":"private@example.test","document":"12345678900"},
						"metadata":{"cart_token":"checkout-token-secret"},
						"charges":[{"id":"ch_safe","paid_at":%q,"last_transaction":{
						"status":%q,"success":%t,"acquirer_message":"private@example.test",
						"acquirer_return_code":"1016","acquirer_auth_code":"auth-safe",
						"gateway_response":{"code":400,"errors":[{"message":"Private Buyer"}]},
						"antifraud_response":{"status":"approved","buyer":"Private Buyer"},
						"card":{"brand":"Mastercard","last_four_digits":"1111","number":"4111111111111111"}
						}}]}`, tc.status, paidAt, tc.transaction, tc.want == PaymentApproved)
				default:
					t.Fatalf("unexpected request: %s", req.URL.Path)
					return 500, ""
				}
			})
			result, err := p.ProcessCardPayment(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 3 || result.Status != tc.want || result.Amount != input.TotalAmount || result.PaymentID != "ch_safe" {
				t.Fatalf("payment behavior changed: calls=%d result=%+v", calls, result)
			}
			if (result.PaidAt != nil) != (tc.want == PaymentApproved) || result.Installments != 3 || result.LastFourDigits != "1111" {
				t.Fatalf("payment result changed: %+v", result)
			}
			if order["metadata"].(map[string]any)["cart_token"] != "checkout-token-secret" {
				t.Fatal("logging removed metadata from provider request")
			}
			entries := logs.FilterMessage("pagarme card payment " + tc.message).All()
			if len(entries) != 1 || entries[0].Level != zapcore.InfoLevel {
				t.Fatalf("wrong outcome or severity: %+v", logs.All())
			}
			fields := entries[0].ContextMap()
			if fields["cart_id"] != input.CartID || fields["payment_id"] != "ch_safe" || fields["acquirer_return_code"] != "1016" {
				t.Fatalf("lost correlation fields: %v", fields)
			}
			assertNoPaymentSecrets(t, logs)
		})
	}
}

func TestPagarmePaymentRequestFailuresDoNotLogPayloads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		status     int
		wantError  bool
	}{
		{name: "customer validation", path: "/core/v5/customers", status: 422, wantError: true},
		{name: "card validation", path: "/core/v5/customers/cus_safe/cards", status: 422, wantError: true},
		{name: "order validation", path: "/core/v5/orders", status: 422},
		{name: "gateway outage", path: "/core/v5/orders", status: 503, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.InfoLevel)
			p := pagarmeLogFixture(t, zap.New(core), func(req *http.Request) (int, string) {
				if req.URL.Path == tc.path {
					return tc.status, `{"message":"validation for private@example.test","customer":{"name":"Private Buyer"},"metadata":{"cart_token":"checkout-token-secret"}}`
				}
				if req.URL.Path == "/core/v5/customers" {
					return 200, `{"id":"cus_safe"}`
				}
				return 200, `{"id":"card_safe"}`
			})
			result, err := p.ProcessCardPayment(t.Context(), sensitiveCardInput())
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if err != nil {
				var safe interface{ LogMessage() string }
				if !errors.As(err, &safe) || !strings.Contains(safe.LogMessage(), fmt.Sprint(tc.status)) {
					t.Fatalf("payment error has no safe status: %v", err)
				}
				if strings.Contains(safe.LogMessage(), "private@example.test") {
					t.Fatal("safe error summary contains customer data")
				}
			}
			if !tc.wantError && result.Status != PaymentRejected {
				t.Fatalf("validation no longer returns rejection: %+v", result)
			}
			assertNoPaymentSecrets(t, logs)
		})
	}
}

func TestPagarmePixLogsDoNotExposePayloadOrQRCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{name: "failed", status: 200, body: `{"id":"or_safe","amount":29025,"charges":[{"id":"ch_safe","status":"failed","last_transaction":{"status":"failed","gateway_response":{"code":400,"errors":[{"message":"Private Buyer"}]}}}]}`, wantError: true},
		{name: "empty qr", status: 200, body: `{"id":"or_safe","amount":29025,"metadata":{"cart_token":"checkout-token-secret"}}`, wantError: true},
		{name: "http error", status: 422, body: `{"message":"private@example.test","customer":{"name":"Private Buyer"}}`, wantError: true},
		{name: "valid", status: 200, body: `{"id":"or_safe","amount":29025,"charges":[{"id":"ch_safe","status":"pending","last_transaction":{"qr_code":"checkout-token-secret","qr_code_url":"https://example.test/checkout-token-secret"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.InfoLevel)
			p := pagarmeLogFixture(t, zap.New(core), func(*http.Request) (int, string) { return tc.status, tc.body })
			input := sensitiveCardInput()
			result, err := p.GeneratePixPayment(t.Context(), PixPaymentInput{
				CartID: input.CartID, TotalAmount: input.TotalAmount, Customer: input.Customer, Metadata: input.Metadata,
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if !tc.wantError && (result.PaymentID != "ch_safe" || result.Amount != 29025 || result.QRCodeText != "checkout-token-secret") {
				t.Fatalf("pix result changed: %+v", result)
			}
			assertNoPaymentSecrets(t, logs)
		})
	}
}

func TestMercadoPagoCardOutcomeAndFailureLogs(t *testing.T) {
	// This SDK path uses a fresh http.Client with DefaultTransport. Replace it
	// only in this serial test; every request is intercepted, never sent out.
	for _, tc := range []struct {
		name, status, outcome string
		httpStatus            int
		want                  PaymentState
	}{
		{name: "approved", status: "approved", httpStatus: 201, want: PaymentApproved, outcome: "captured"},
		{name: "rejected", status: "rejected", httpStatus: 201, want: PaymentRejected, outcome: "rejected"},
		{name: "pending", status: "pending", httpStatus: 201, want: PaymentPending, outcome: "pending"},
		{name: "http validation", httpStatus: 400, want: PaymentRejected, outcome: "request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = statusTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "api.mercadopago.com" || req.URL.Path != "/v1/payments" {
					t.Fatalf("unexpected SDK request %s", req.URL)
				}
				body := fmt.Sprintf(`{"id":123,"status":%q,"transaction_amount":290.25,"installments":3,"payer":{"email":"private@example.test"},"metadata":{"cart_token":"checkout-token-secret"}}`, tc.status)
				if tc.httpStatus == 400 {
					body = `{"message":"Private Buyer private@example.test","status":400,"cause":[],"metadata":{"cart_token":"checkout-token-secret"}}`
				}
				return &http.Response{StatusCode: tc.httpStatus, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			core, logs := observer.New(zap.InfoLevel)
			p, err := NewMercadoPago(MercadoPagoConfig{Credentials: &Credentials{AccessToken: "test"}, Logger: zap.New(core)})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.ProcessCardPayment(t.Context(), sensitiveCardInput())
			if err != nil || result.Status != tc.want {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if logs.FilterMessage("mercado pago card payment "+tc.outcome).Len() != 1 {
				t.Fatalf("wrong outcome logged: %v", logs.All())
			}
			assertNoPaymentSecrets(t, logs)
		})
	}
}

func TestMercadoPagoResponseErrorKeepsCauseAndSafeSummary(t *testing.T) {
	t.Parallel()
	original := &mperror.ResponseError{StatusCode: 422, Message: "invalid customer private@example.test"}
	err := mercadoPagoError("creating checkout", original)
	var cause *mperror.ResponseError
	if !errors.As(err, &cause) || cause != original {
		t.Fatal("provider error identity was lost")
	}
	var safe interface{ LogMessage() string }
	if !errors.As(err, &safe) || safe.LogMessage() != "mercado pago creating checkout: HTTP 422" {
		t.Fatalf("missing safe summary: %v", err)
	}
	if err.Error() != "creating checkout: "+original.Message {
		t.Fatal("caller-facing error changed")
	}
	if got := parsePagarmeError([]byte("private@example.test")); strings.Contains(got, "private@example.test") {
		t.Fatal("unrecognized raw provider body was surfaced")
	}
}
