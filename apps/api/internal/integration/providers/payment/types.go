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

	// Transparent checkout types
	CardPaymentInput   = providers.CardPaymentInput
	CardPaymentResult  = providers.CardPaymentResult
	PixPaymentInput    = providers.PixPaymentInput
	PixPaymentResult   = providers.PixPaymentResult
	CheckoutConfigResult = providers.CheckoutConfigResult
	PayerCostInfo      = providers.PayerCostInfo
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
