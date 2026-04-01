package erp

import (
	"livecart/apps/api/internal/integration/providers"
)

// Re-export types from providers package for convenience
type (
	ERPOrder             = providers.ERPOrder
	ERPOrderItem         = providers.ERPOrderItem
	OrderResult          = providers.OrderResult
	ListProductsParams   = providers.ListProductsParams
	ProductListResult    = providers.ProductListResult
	ERPProduct           = providers.ERPProduct
	SyncResult           = providers.SyncResult
	Credentials          = providers.Credentials
	WebhookEvent         = providers.WebhookEvent
	SearchContactsParams = providers.SearchContactsParams
	ERPContactInput      = providers.ERPContactInput
	ERPContactResult     = providers.ERPContactResult
)
