package shipping

import "livecart/apps/api/internal/integration/providers"

// Re-export types from providers package for convenience.
type (
	Credentials    = providers.Credentials
	QuoteRequest   = providers.QuoteRequest
	QuoteOption    = providers.QuoteOption
	ShippingItem   = providers.ShippingItem
	ShippingZip    = providers.ShippingZip
	CarrierService = providers.CarrierService
)
