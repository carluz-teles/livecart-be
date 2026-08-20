package payment_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
)

// fakeResolver is a scripted payment.IntegrationResolver. handled is a pointer
// so the recorded HandleProviderError calls survive the value copies the
// fakeResolver goes through when passed into NewService.
type fakeResolver struct {
	integrationType string
	build           func() (providers.Provider, error)
	err             error
	handled         *[]string
}

func (f fakeResolver) ResolveIntegration(_ context.Context, _, _ string) (string, func() (providers.Provider, error), error) {
	return f.integrationType, f.build, f.err
}

func (f fakeResolver) HandleProviderError(_ context.Context, _, operation string, _ error) {
	if f.handled != nil {
		*f.handled = append(*f.handled, operation)
	}
}

// fakePaymentProvider satisfies providers.PaymentProvider through an embedded
// (nil) interface — GetProvider only casts it, it never calls its methods.
type fakePaymentProvider struct{ providers.PaymentProvider }

// fakeBaseProvider satisfies providers.Provider but NOT
// providers.PaymentProvider, exercising the cast-failure path.
type fakeBaseProvider struct{ providers.Provider }

// stubPaymentProvider is a scripted providers.PaymentProvider used by the
// fast-path tests. It embeds the interface (nil) so only the methods a given
// test needs are implemented; calling any other method would panic, which
// keeps the tests honest about which provider calls each fast-path makes.
type stubPaymentProvider struct {
	providers.PaymentProvider

	refundResult *providers.RefundResult
	refundErr    error

	publicKey    string
	publicKeyErr error
	methods      []string
	methodsErr   error

	// Fast-path scripting (B1b). name is returned by Name(), which
	// CreateCheckout calls when it has to build the notify URL itself.
	name           providers.ProviderName
	checkoutResult *providers.CheckoutResult
	checkoutErr    error
	statusResult   *providers.PaymentStatus
	statusErr      error
	cardResult     *providers.CardPaymentResult
	cardErr        error
	pixResult      *providers.PixPaymentResult
	pixErr         error

	// Capture pointers record the value each fast-path hands to the provider,
	// so a test can assert the field-by-field mapping the fast-path performs
	// (e.g. ProcessCardPaymentInput.CardToken -> CardPaymentInput.Token). Value
	// receivers copy the struct, but the pointers still target the caller's var.
	gotCheckoutOrder *providers.CheckoutOrder
	gotCardInput     *providers.CardPaymentInput
	gotPixInput      *providers.PixPaymentInput
}

func (p stubPaymentProvider) Name() providers.ProviderName {
	return p.name
}

func (p stubPaymentProvider) RefundPayment(_ context.Context, _ string, _ *int64) (*providers.RefundResult, error) {
	return p.refundResult, p.refundErr
}

func (p stubPaymentProvider) GetPublicKey(_ context.Context) (string, error) {
	return p.publicKey, p.publicKeyErr
}

func (p stubPaymentProvider) GetPaymentMethods(_ context.Context) ([]string, error) {
	return p.methods, p.methodsErr
}

func (p stubPaymentProvider) CreateCheckout(_ context.Context, order providers.CheckoutOrder) (*providers.CheckoutResult, error) {
	if p.gotCheckoutOrder != nil {
		*p.gotCheckoutOrder = order
	}
	return p.checkoutResult, p.checkoutErr
}

func (p stubPaymentProvider) GetPaymentStatus(_ context.Context, _ string) (*providers.PaymentStatus, error) {
	return p.statusResult, p.statusErr
}

func (p stubPaymentProvider) ProcessCardPayment(_ context.Context, input providers.CardPaymentInput) (*providers.CardPaymentResult, error) {
	if p.gotCardInput != nil {
		*p.gotCardInput = input
	}
	return p.cardResult, p.cardErr
}

func (p stubPaymentProvider) GeneratePixPayment(_ context.Context, input providers.PixPaymentInput) (*providers.PixPaymentResult, error) {
	if p.gotPixInput != nil {
		*p.gotPixInput = input
	}
	return p.pixResult, p.pixErr
}

// fakeIdempotencyRepo is a scripted idempotency.Repository. GetByKey returns
// byKey (a completed record drives the cache-hit replay); Create returns a
// fixed record so the fast-path enters the Start/Complete branch; Update
// records the status transitions so a test can assert "completed" vs "failed".
type fakeIdempotencyRepo struct {
	byKey   *idempotency.Record
	created *idempotency.Record
	updates *[]string
}

func (r fakeIdempotencyRepo) GetByKey(_ context.Context, _, _ string) (*idempotency.Record, error) {
	return r.byKey, nil
}

func (r fakeIdempotencyRepo) GetByHash(_ context.Context, _, _ string, _ time.Time) (*idempotency.Record, error) {
	return nil, nil
}

func (r fakeIdempotencyRepo) Create(_ context.Context, _ idempotency.CreateParams) (*idempotency.Record, error) {
	return r.created, nil
}

func (r fakeIdempotencyRepo) Update(_ context.Context, _ string, _ []byte, status string) error {
	if r.updates != nil {
		*r.updates = append(*r.updates, status)
	}
	return nil
}

func (r fakeIdempotencyRepo) Reclaim(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func TestService_GetProvider(t *testing.T) {
	t.Parallel()

	paymentProvider := fakePaymentProvider{}
	notFound := httpx.ErrNotFound("integration not found")
	buildErr := errors.New("building provider failed")

	tests := []struct {
		name         string
		resolver     fakeResolver
		wantErrIs    error  // exact error expected (identity); nil = skip
		wantErrMsg   string // substring of error message; "" = skip
		wantProvider bool
	}{
		{
			name: "resolves payment provider",
			resolver: fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return paymentProvider, nil },
			},
			wantProvider: true,
		},
		{
			name: "rejects non-payment integration",
			resolver: fakeResolver{
				integrationType: string(providers.ProviderTypeERP),
				build:           func() (providers.Provider, error) { return paymentProvider, nil },
			},
			wantErrMsg: "integration is not a payment provider",
		},
		{
			name:      "propagates resolver error",
			resolver:  fakeResolver{err: notFound},
			wantErrIs: notFound,
		},
		{
			name: "fails cast when provider is not a payment provider",
			resolver: fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return fakeBaseProvider{}, nil },
			},
			wantErrMsg: "failed to cast to payment provider",
		},
		{
			name: "propagates builder error",
			resolver: fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return nil, buildErr },
			},
			wantErrIs: buildErr,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := payment.NewService(tt.resolver, nil, zap.NewNop())
			got, err := svc.GetProvider(context.Background(), "integration-id", "store-id")

			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("GetProvider() error = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrMsg != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("GetProvider() error = %v, want message containing %q", err, tt.wantErrMsg)
				}
			default:
				if err != nil {
					t.Fatalf("GetProvider() unexpected error: %v", err)
				}
			}

			if tt.wantProvider && got == nil {
				t.Fatal("GetProvider() returned nil provider, want non-nil")
			}
			if !tt.wantProvider && got != nil {
				t.Fatalf("GetProvider() returned provider %v, want nil", got)
			}
		})
	}
}

func TestService_CreateCheckout(t *testing.T) {
	// No t.Parallel(): the notify-URL case uses t.Setenv, which forbids it.

	cachedOutput := payment.CreateCheckoutOutput{CheckoutID: "chk_cached", CheckoutURL: "https://cached"}
	cachedJSON, err := json.Marshal(cachedOutput)
	if err != nil {
		t.Fatalf("marshaling cached output: %v", err)
	}

	tests := []struct {
		name               string
		idemKey            string
		byKey              *idempotency.Record
		provider           stubPaymentProvider
		webhookBase        string
		notifyURL          string
		wantCheckoutID     string
		wantProviderCalled bool
		wantNotifyURL      string // "" = skip
		wantUpdateStatus   string // last idempotency status; "" = no Update expected
	}{
		{
			name:               "replays cached response without calling the provider",
			idemKey:            "idem-1",
			byKey:              &idempotency.Record{Status: "completed", Response: cachedJSON},
			wantCheckoutID:     "chk_cached",
			wantProviderCalled: false,
		},
		{
			name:               "creates checkout and completes idempotency on the normal path",
			provider:           stubPaymentProvider{checkoutResult: &providers.CheckoutResult{CheckoutID: "chk_new", CheckoutURL: "https://new"}},
			notifyURL:          "https://caller/webhook",
			wantCheckoutID:     "chk_new",
			wantProviderCalled: true,
			wantNotifyURL:      "https://caller/webhook",
			wantUpdateStatus:   "completed",
		},
		{
			name:               "builds notify URL from provider name when caller omits it",
			provider:           stubPaymentProvider{name: providers.ProviderName("mercado_pago"), checkoutResult: &providers.CheckoutResult{CheckoutID: "chk_notify"}},
			webhookBase:        "https://api.test",
			wantCheckoutID:     "chk_notify",
			wantProviderCalled: true,
			wantNotifyURL:      "https://api.test/api/webhooks/mercado_pago/store-id",
			wantUpdateStatus:   "completed",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.webhookBase != "" {
				t.Setenv("WEBHOOK_BASE_URL", tt.webhookBase)
			}

			var gotOrder providers.CheckoutOrder
			prov := tt.provider
			prov.gotCheckoutOrder = &gotOrder

			built := false
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build: func() (providers.Provider, error) {
					built = true
					return prov, nil
				},
			}
			updates := []string{}
			repo := fakeIdempotencyRepo{
				byKey:   tt.byKey,
				created: &idempotency.Record{ID: "rec-1"},
				updates: &updates,
			}
			svc := payment.NewService(resolver, idempotency.NewService(repo), zap.NewNop())

			out, err := svc.CreateCheckout(context.Background(), payment.CreateCheckoutInput{
				StoreID:        "store-id",
				IntegrationID:  "integration-id",
				IdempotencyKey: tt.idemKey,
				CartID:         "cart-1",
				NotifyURL:      tt.notifyURL,
			})
			if err != nil {
				t.Fatalf("CreateCheckout() unexpected error: %v", err)
			}
			if out == nil || out.CheckoutID != tt.wantCheckoutID {
				t.Fatalf("CreateCheckout() output = %+v, want CheckoutID %q", out, tt.wantCheckoutID)
			}
			if built != tt.wantProviderCalled {
				t.Fatalf("provider built = %v, want %v", built, tt.wantProviderCalled)
			}
			if tt.wantNotifyURL != "" && gotOrder.NotifyURL != tt.wantNotifyURL {
				t.Fatalf("CheckoutOrder.NotifyURL = %q, want %q", gotOrder.NotifyURL, tt.wantNotifyURL)
			}

			gotUpdate := ""
			if len(updates) > 0 {
				gotUpdate = updates[len(updates)-1]
			}
			if gotUpdate != tt.wantUpdateStatus {
				t.Fatalf("idempotency Update status = %q, want %q", gotUpdate, tt.wantUpdateStatus)
			}
		})
	}
}

func TestService_GetPaymentStatus(t *testing.T) {
	t.Parallel()

	statusErr := errors.New("provider status lookup failed")
	notFound := httpx.ErrNotFound("integration not found")

	tests := []struct {
		name       string
		provider   stubPaymentProvider
		resolveErr error
		wantStatus string
		wantAmount int64
		wantErrIs  error  // identity; nil = skip
		wantErrMsg string // substring; "" = skip
		wantHandle bool   // HandleProviderError expected with "get_payment_status"
	}{
		{
			name: "returns mapped status on success",
			provider: stubPaymentProvider{statusResult: &providers.PaymentStatus{
				PaymentID: "pay_1",
				Status:    providers.PaymentApproved,
				Amount:    2500,
			}},
			wantStatus: "approved",
			wantAmount: 2500,
		},
		{
			name:       "wraps provider error and flags integration",
			provider:   stubPaymentProvider{statusErr: statusErr},
			wantErrMsg: "getting payment status",
			wantHandle: true,
		},
		{
			name:       "propagates resolver error without flagging",
			resolveErr: notFound,
			wantErrIs:  notFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handled := []string{}
			prov := tt.provider
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return prov, nil },
				err:             tt.resolveErr,
				handled:         &handled,
			}
			svc := payment.NewService(resolver, nil, zap.NewNop())

			out, err := svc.GetPaymentStatus(context.Background(), payment.GetPaymentStatusInput{
				StoreID:       "store-id",
				IntegrationID: "integration-id",
				PaymentID:     "pay-id",
			})

			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("GetPaymentStatus() error = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrMsg != "":
				// D1e-2: the provider failure is now wrapped in
				// httpx.InfrastructureError (generic 500, category INFRASTRUCTURE)
				// instead of a descriptive fmt.Errorf — Error() is the client-safe
				// "internal server error" and never leaks the provider detail, while
				// the original provider error stays recoverable via the chain.
				var se *httpx.ServiceError
				if !errors.As(err, &se) || se.Category != httpx.CategoryInfrastructure {
					t.Fatalf("GetPaymentStatus() error = %v, want an INFRASTRUCTURE ServiceError", err)
				}
				if !errors.Is(err, statusErr) {
					t.Fatalf("GetPaymentStatus() error = %v, want the provider cause recoverable", err)
				}
			default:
				if err != nil {
					t.Fatalf("GetPaymentStatus() unexpected error: %v", err)
				}
				if out == nil || out.Status != tt.wantStatus || out.Amount != tt.wantAmount {
					t.Fatalf("GetPaymentStatus() output = %+v, want status %q amount %d", out, tt.wantStatus, tt.wantAmount)
				}
			}

			gotHandle := len(handled) == 1 && handled[0] == "get_payment_status"
			if gotHandle != tt.wantHandle {
				t.Fatalf("HandleProviderError calls = %v, want handled=%v", handled, tt.wantHandle)
			}
		})
	}
}

func TestService_RefundPayment(t *testing.T) {
	t.Parallel()

	refundErr := errors.New("provider refused refund")
	notFound := httpx.ErrNotFound("integration not found")

	tests := []struct {
		name       string
		provider   stubPaymentProvider
		resolveErr error
		wantAmount int64
		wantErrIs  error  // identity match; nil = skip
		wantErrMsg string // substring; "" = skip
		wantHandle bool   // HandleProviderError expected with "refund_payment"
	}{
		{
			name: "returns refund result on success",
			provider: stubPaymentProvider{
				refundResult: &providers.RefundResult{RefundID: "re_1", Status: "refunded", Amount: 1500},
			},
			wantAmount: 1500,
		},
		{
			name:       "wraps provider error and flags integration",
			provider:   stubPaymentProvider{refundErr: refundErr},
			wantErrMsg: "refunding payment",
			wantHandle: true,
		},
		{
			name:       "propagates resolver error without flagging",
			resolveErr: notFound,
			wantErrIs:  notFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handled := []string{}
			prov := tt.provider
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return prov, nil },
				err:             tt.resolveErr,
				handled:         &handled,
			}
			svc := payment.NewService(resolver, nil, zap.NewNop())

			out, err := svc.RefundPayment(context.Background(), payment.RefundPaymentInput{
				StoreID:       "store-id",
				IntegrationID: "integration-id",
				PaymentID:     "pay-id",
			})

			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("RefundPayment() error = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrMsg != "":
				// D1e-2: provider failure wrapped in httpx.InfrastructureError —
				// generic 500 (category INFRASTRUCTURE), provider detail never
				// leaked via Error(), original cause recoverable via the chain.
				var se *httpx.ServiceError
				if !errors.As(err, &se) || se.Category != httpx.CategoryInfrastructure {
					t.Fatalf("RefundPayment() error = %v, want an INFRASTRUCTURE ServiceError", err)
				}
				if !errors.Is(err, refundErr) {
					t.Fatalf("RefundPayment() error = %v, want the provider cause recoverable", err)
				}
			default:
				if err != nil {
					t.Fatalf("RefundPayment() unexpected error: %v", err)
				}
				if out == nil || out.Amount != tt.wantAmount {
					t.Fatalf("RefundPayment() output = %+v, want amount %d", out, tt.wantAmount)
				}
			}

			gotHandle := len(handled) == 1 && handled[0] == "refund_payment"
			if gotHandle != tt.wantHandle {
				t.Fatalf("HandleProviderError calls = %v, want handled=%v", handled, tt.wantHandle)
			}
		})
	}
}

func TestService_GetCheckoutConfig(t *testing.T) {
	t.Parallel()

	keyErr := errors.New("provider key unavailable")

	tests := []struct {
		name        string
		provider    stubPaymentProvider
		wantKey     string
		wantMethods []string
		wantErrMsg  string // substring; "" = skip
		wantHandle  string // operation expected on HandleProviderError; "" = none
	}{
		{
			name:        "returns public key and methods on success",
			provider:    stubPaymentProvider{publicKey: "pk_test", methods: []string{"pix", "credit_card"}},
			wantKey:     "pk_test",
			wantMethods: []string{"pix", "credit_card"},
		},
		{
			name:       "wraps public-key error and flags integration",
			provider:   stubPaymentProvider{publicKeyErr: keyErr},
			wantErrMsg: "getting public key",
			wantHandle: "get_public_key",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handled := []string{}
			prov := tt.provider
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return prov, nil },
				handled:         &handled,
			}
			svc := payment.NewService(resolver, nil, zap.NewNop())

			key, methods, err := svc.GetCheckoutConfig(context.Background(), "integration-id", "store-id")

			if tt.wantErrMsg != "" {
				// D1e-2: provider failure wrapped in httpx.InfrastructureError —
				// generic 500 (category INFRASTRUCTURE), provider detail never
				// leaked via Error(), original cause recoverable via the chain.
				var se *httpx.ServiceError
				if !errors.As(err, &se) || se.Category != httpx.CategoryInfrastructure {
					t.Fatalf("GetCheckoutConfig() error = %v, want an INFRASTRUCTURE ServiceError", err)
				}
				if !errors.Is(err, keyErr) {
					t.Fatalf("GetCheckoutConfig() error = %v, want the provider cause recoverable", err)
				}
			} else {
				if err != nil {
					t.Fatalf("GetCheckoutConfig() unexpected error: %v", err)
				}
				if key != tt.wantKey {
					t.Fatalf("GetCheckoutConfig() key = %q, want %q", key, tt.wantKey)
				}
				if strings.Join(methods, ",") != strings.Join(tt.wantMethods, ",") {
					t.Fatalf("GetCheckoutConfig() methods = %v, want %v", methods, tt.wantMethods)
				}
			}

			switch tt.wantHandle {
			case "":
				if len(handled) != 0 {
					t.Fatalf("HandleProviderError called %v, want none", handled)
				}
			default:
				if len(handled) != 1 || handled[0] != tt.wantHandle {
					t.Fatalf("HandleProviderError calls = %v, want [%q]", handled, tt.wantHandle)
				}
			}
		})
	}
}

func TestService_ProcessCardPayment(t *testing.T) {
	t.Parallel()

	processErr := errors.New("provider declined card")
	notFound := httpx.ErrNotFound("integration not found")

	tests := []struct {
		name       string
		provider   stubPaymentProvider
		resolveErr error
		wantStatus string
		wantErrIs  error  // identity; nil = skip
		wantErrMsg string // substring; "" = skip
		wantHandle bool   // HandleProviderError expected with "process_card_payment"
	}{
		{
			name: "maps input and returns result on success",
			provider: stubPaymentProvider{cardResult: &providers.CardPaymentResult{
				PaymentID:    "pay_1",
				Status:       providers.PaymentApproved,
				Amount:       5000,
				Installments: 3,
			}},
			wantStatus: "approved",
		},
		{
			name:       "wraps provider error and flags integration",
			provider:   stubPaymentProvider{cardErr: processErr},
			wantErrMsg: "processing card payment",
			wantHandle: true,
		},
		{
			name:       "propagates resolver error without flagging",
			resolveErr: notFound,
			wantErrIs:  notFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handled := []string{}
			var gotInput providers.CardPaymentInput
			prov := tt.provider
			prov.gotCardInput = &gotInput
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return prov, nil },
				err:             tt.resolveErr,
				handled:         &handled,
			}
			svc := payment.NewService(resolver, nil, zap.NewNop())

			out, err := svc.ProcessCardPayment(context.Background(), payment.ProcessCardPaymentInput{
				StoreID:         "store-id",
				IntegrationID:   "integration-id",
				CartID:          "cart-1",
				CardToken:       "tok_123",
				Installments:    3,
				TotalAmount:     5000,
				Currency:        "BRL",
				NotifyURL:       "https://notify",
				PaymentMethodID: "visa",
				IssuerID:        "issuer-1",
				DeviceID:        "dev-1",
			})

			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("ProcessCardPayment() error = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrMsg != "":
				// D1e-2: provider failure wrapped in httpx.InfrastructureError —
				// generic 500 (category INFRASTRUCTURE), provider detail never
				// leaked via Error(), original cause recoverable via the chain.
				var se *httpx.ServiceError
				if !errors.As(err, &se) || se.Category != httpx.CategoryInfrastructure {
					t.Fatalf("ProcessCardPayment() error = %v, want an INFRASTRUCTURE ServiceError", err)
				}
				if !errors.Is(err, processErr) {
					t.Fatalf("ProcessCardPayment() error = %v, want the provider cause recoverable", err)
				}
			default:
				if err != nil {
					t.Fatalf("ProcessCardPayment() unexpected error: %v", err)
				}
				if out == nil || out.Status != tt.wantStatus {
					t.Fatalf("ProcessCardPayment() output = %+v, want status %q", out, tt.wantStatus)
				}
				// Guard the field-by-field mapping the fast-path performs — the
				// exact regression the review flagged (CardToken -> Token, and
				// the provider-specific IDs that would silently drop).
				if gotInput.Token != "tok_123" {
					t.Fatalf("CardPaymentInput.Token = %q, want the mapped CardToken %q", gotInput.Token, "tok_123")
				}
				if gotInput.CartID != "cart-1" ||
					gotInput.Installments != 3 ||
					gotInput.TotalAmount != 5000 ||
					gotInput.Currency != "BRL" ||
					gotInput.NotifyURL != "https://notify" ||
					gotInput.PaymentMethodID != "visa" ||
					gotInput.IssuerID != "issuer-1" ||
					gotInput.DeviceID != "dev-1" {
					t.Fatalf("CardPaymentInput mapping mismatch: %+v", gotInput)
				}
			}

			gotHandle := len(handled) == 1 && handled[0] == "process_card_payment"
			if gotHandle != tt.wantHandle {
				t.Fatalf("HandleProviderError calls = %v, want handled=%v", handled, tt.wantHandle)
			}
		})
	}
}

func TestService_GeneratePixPayment(t *testing.T) {
	t.Parallel()

	pixErr := errors.New("provider could not generate pix")
	notFound := httpx.ErrNotFound("integration not found")

	tests := []struct {
		name       string
		provider   stubPaymentProvider
		resolveErr error
		wantQRText string
		wantErrIs  error  // identity; nil = skip
		wantErrMsg string // substring; "" = skip
		wantHandle bool   // HandleProviderError expected with "generate_pix_payment"
	}{
		{
			name: "maps input and returns result on success",
			provider: stubPaymentProvider{pixResult: &providers.PixPaymentResult{
				PaymentID:  "pay_pix_1",
				QRCode:     "base64-image",
				QRCodeText: "copia-e-cola",
				Amount:     3200,
			}},
			wantQRText: "copia-e-cola",
		},
		{
			name:       "wraps provider error and flags integration",
			provider:   stubPaymentProvider{pixErr: pixErr},
			wantErrMsg: "generating pix payment",
			wantHandle: true,
		},
		{
			name:       "propagates resolver error without flagging",
			resolveErr: notFound,
			wantErrIs:  notFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handled := []string{}
			var gotInput providers.PixPaymentInput
			prov := tt.provider
			prov.gotPixInput = &gotInput
			resolver := fakeResolver{
				integrationType: string(providers.ProviderTypePayment),
				build:           func() (providers.Provider, error) { return prov, nil },
				err:             tt.resolveErr,
				handled:         &handled,
			}
			svc := payment.NewService(resolver, nil, zap.NewNop())

			out, err := svc.GeneratePixPayment(context.Background(), payment.GeneratePixPaymentInput{
				StoreID:       "store-id",
				IntegrationID: "integration-id",
				CartID:        "cart-1",
				TotalAmount:   3200,
				Currency:      "BRL",
				NotifyURL:     "https://notify",
			})

			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("GeneratePixPayment() error = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrMsg != "":
				// D1e-2: provider failure wrapped in httpx.InfrastructureError —
				// generic 500 (category INFRASTRUCTURE), provider detail never
				// leaked via Error(), original cause recoverable via the chain.
				var se *httpx.ServiceError
				if !errors.As(err, &se) || se.Category != httpx.CategoryInfrastructure {
					t.Fatalf("GeneratePixPayment() error = %v, want an INFRASTRUCTURE ServiceError", err)
				}
				if !errors.Is(err, pixErr) {
					t.Fatalf("GeneratePixPayment() error = %v, want the provider cause recoverable", err)
				}
			default:
				if err != nil {
					t.Fatalf("GeneratePixPayment() unexpected error: %v", err)
				}
				if out == nil || out.QRCodeText != tt.wantQRText {
					t.Fatalf("GeneratePixPayment() output = %+v, want QRCodeText %q", out, tt.wantQRText)
				}
				if gotInput.CartID != "cart-1" ||
					gotInput.TotalAmount != 3200 ||
					gotInput.Currency != "BRL" ||
					gotInput.NotifyURL != "https://notify" {
					t.Fatalf("PixPaymentInput mapping mismatch: %+v", gotInput)
				}
			}

			gotHandle := len(handled) == 1 && handled[0] == "generate_pix_payment"
			if gotHandle != tt.wantHandle {
				t.Fatalf("HandleProviderError calls = %v, want handled=%v", handled, tt.wantHandle)
			}
		})
	}
}
