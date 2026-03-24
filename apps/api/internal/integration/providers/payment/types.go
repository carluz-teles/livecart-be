package payment

import (
	"livecart/apps/api/internal/integration/providers"
)

// Re-export types from providers package for convenience
type (
	CheckoutOrder    = providers.CheckoutOrder
	CheckoutItem     = providers.CheckoutItem
	CheckoutCustomer = providers.CheckoutCustomer
	CheckoutResult   = providers.CheckoutResult
	PaymentStatus    = providers.PaymentStatus
	PaymentState     = providers.PaymentState
	RefundResult     = providers.RefundResult
	Credentials      = providers.Credentials
	WebhookEvent     = providers.WebhookEvent
)

// Re-export constants
const (
	PaymentPending   = providers.PaymentPending
	PaymentApproved  = providers.PaymentApproved
	PaymentRejected  = providers.PaymentRejected
	PaymentRefunded  = providers.PaymentRefunded
	PaymentCancelled = providers.PaymentCancelled
	PaymentInProcess = providers.PaymentInProcess
)
