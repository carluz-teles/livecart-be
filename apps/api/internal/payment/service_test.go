package payment_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
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
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("RefundPayment() error = %v, want message containing %q", err, tt.wantErrMsg)
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
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("GetCheckoutConfig() error = %v, want message containing %q", err, tt.wantErrMsg)
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
