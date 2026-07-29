package payment_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
)

// fakeResolver is a scripted payment.IntegrationResolver.
type fakeResolver struct {
	integrationType string
	build           func() (providers.Provider, error)
	err             error
}

func (f fakeResolver) ResolveIntegration(_ context.Context, _, _ string) (string, func() (providers.Provider, error), error) {
	return f.integrationType, f.build, f.err
}

// fakePaymentProvider satisfies providers.PaymentProvider through an embedded
// (nil) interface — GetProvider only casts it, it never calls its methods.
type fakePaymentProvider struct{ providers.PaymentProvider }

// fakeBaseProvider satisfies providers.Provider but NOT
// providers.PaymentProvider, exercising the cast-failure path.
type fakeBaseProvider struct{ providers.Provider }

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

			svc := payment.NewService(tt.resolver)
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
