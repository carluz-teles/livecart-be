package payment

import (
	"context"
	"go.uber.org/zap"
	"io"
	"livecart/apps/api/internal/integration/providers"
	"net/http"
	"strings"
	"testing"
)

type statusTransport func(*http.Request) (*http.Response, error)

func (f statusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPagarmeStatus_OrderAndChargeHaveOnePaymentIdentity(t *testing.T) {
	const charge = `{"id":"ch_one","code":"cart_one","status":"paid","amount":8290,"payment_method":"pix"}`
	p, err := NewPagarme(PagarmeConfig{Credentials: &Credentials{APIKey: "test"}, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	p.HTTPClient = &http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
		body := charge
		if strings.Contains(r.URL.Path, "/orders/") {
			body = `{"id":"or_one","code":"cart_one","status":"paid","amount":8290,"charges":[` + charge + `]}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, id := range []string{"or_one", "ch_one"} {
		t.Run(id, func(t *testing.T) {
			got, err := p.GetPaymentStatus(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if got.PaymentID != "ch_one" || got.Amount != 8290 || got.Status != providers.PaymentApproved {
				t.Fatalf("status: %+v", got)
			}
		})
	}
}

func TestPagarmeCancelPixPayment_NeverUsesRefundEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"pending", `{"status":"pending","last_transaction":{"expires_at":"2099-01-01T00:00:00Z"}}`, true},
		{"paid", `{"status":"paid"}`, true},
		{"expired", `{"status":"pending","last_transaction":{"expires_at":"2000-01-01T00:00:00Z"}}`, false},
		{"cancelled", `{"status":"canceled"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPagarme(PagarmeConfig{Credentials: &Credentials{APIKey: "test"}, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			p.HTTPClient = &http.Client{Transport: statusTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet {
					t.Fatalf("automatic invalidation issued %s", req.Method)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			err = p.CancelPixPayment(context.Background(), "ch_test")
			if (err != nil) != tc.wantError || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestPagarmeCharge_RecoversAttemptFromParentWithoutChangingCharge(t *testing.T) {
	p, err := NewPagarme(PagarmeConfig{Credentials: &Credentials{APIKey: "test"}, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	p.HTTPClient = &http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/core/v5/charges/ch_second":
			body = `{"id":"ch_second","code":"gateway-code","amount":1000,"status":"paid","order":{"id":"or_parent","code":"cart_one"}}`
		case "/core/v5/orders/or_parent":
			body = `{"id":"or_parent","code":"cart_one","metadata":{"livecart_attempt_id":"attempt_one"},"charges":[{"id":"ch_first","amount":3000},{"id":"ch_second","amount":1000}]}`
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	got, err := p.GetPaymentStatus(context.Background(), "ch_second")
	if err != nil {
		t.Fatal(err)
	}
	if got.PaymentID != "ch_second" || got.Amount != 1000 || got.ExternalReference != "cart_one" || got.Metadata["livecart_attempt_id"] != "attempt_one" {
		t.Fatalf("wrong payment identity: %+v", got)
	}
}
