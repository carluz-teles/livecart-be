// Package payment owns payment-provider resolution for integrations. It is the
// first slice (B1a) of the strangler-fig extraction of the payment domain out
// of the internal/integration monolith. The former CRUD service over the
// vestigial `payments` table (dead code, 0 imports) has been removed and
// replaced by this package.
package payment

import (
	"context"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// Service resolves payment providers for integrations.
type Service struct {
	resolver IntegrationResolver
}

// NewService builds the payment Service. resolver is implemented by
// integration.Service, which owns the shared provider-construction logic.
func NewService(resolver IntegrationResolver) *Service {
	return &Service{resolver: resolver}
}

// GetProvider returns a PaymentProvider for the given integration.
//
// It mirrors the former integration.Service.GetPaymentProvider exactly: fetch
// the integration, reject it if it is not a payment integration, build the
// provider, then cast it to providers.PaymentProvider — surfacing the same
// httpx.ErrUnprocessable errors on the not-a-payment and cast-failure paths.
func (s *Service) GetProvider(ctx context.Context, integrationID, storeID string) (providers.PaymentProvider, error) {
	integrationType, build, err := s.resolver.ResolveIntegration(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integrationType != string(providers.ProviderTypePayment) {
		return nil, httpx.ErrUnprocessable("integration is not a payment provider")
	}

	provider, err := build()
	if err != nil {
		return nil, err
	}

	paymentProvider, ok := provider.(providers.PaymentProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to payment provider")
	}

	return paymentProvider, nil
}
