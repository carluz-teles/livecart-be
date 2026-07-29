package payment

import (
	"context"

	"livecart/apps/api/internal/integration/providers"
)

// IntegrationResolver fetches an integration and lazily builds its provider.
//
// It is declared here — in the consumer package — and implemented by
// integration.Service. Keeping the interface on this side lets the payment
// package resolve providers without importing integration, so integration can
// depend on payment (for the GetPaymentProvider delegation) with no import
// cycle. It intentionally speaks only in terms of the leaf `providers` package.
type IntegrationResolver interface {
	// ResolveIntegration fetches the integration identified by (integrationID,
	// storeID) and returns its declared provider type together with a builder
	// that constructs the initialized provider on demand.
	//
	// The builder is returned rather than the provider itself so the caller can
	// validate the integration type BEFORE any credential decryption / token
	// refresh happens — preserving the exact ordering (and error surface) of the
	// original integration.Service.GetPaymentProvider.
	ResolveIntegration(ctx context.Context, integrationID, storeID string) (integrationType string, build func() (providers.Provider, error), err error)
}
