package payment

import (
	"context"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

// IntegrationResolver is the slice of integration.Service the payment package
// depends on. It is declared here — in the consumer package — and implemented
// by integration.Service. Keeping the interface on this side lets the payment
// package resolve providers and report provider failures without importing
// integration, so integration can depend on payment (for the fast-path
// delegations) with no import cycle. It intentionally speaks only in terms of
// the leaf `providers` package plus primitive types.
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

	// HandleProviderError reports a provider-call failure for the integration
	// (flagging it unhealthy on rate-limit errors). It is owned by
	// integration.Service because the same logic is shared with the ERP/Social
	// fast-paths; it is exposed here so the extracted payment fast-paths (B1b)
	// can report failures without owning — or duplicating — that logic.
	HandleProviderError(ctx context.Context, integrationID, operation string, err error)
}

// CreateCheckoutInput is the service input for creating a checkout.
type CreateCheckoutInput struct {
	StoreID        string
	IntegrationID  string
	IdempotencyKey string
	CartID         string
	Items          []providers.CheckoutItem
	Customer       providers.CheckoutCustomer
	TotalAmount    int64
	Currency       string
	NotifyURL      string
	SuccessURL     string
	FailureURL     string
	Metadata       map[string]any
}

// CreateCheckoutOutput is the service output for creating a checkout.
type CreateCheckoutOutput struct {
	CheckoutID  string
	CheckoutURL string
	ExpiresAt   *time.Time
}

// GetPaymentStatusInput is the service input for getting payment status.
type GetPaymentStatusInput struct {
	StoreID       string
	IntegrationID string
	PaymentID     string
}

// GetPaymentStatusOutput is the service output for getting payment status.
type GetPaymentStatusOutput struct {
	PaymentID     string
	Status        string
	Amount        int64
	PaidAt        *time.Time
	RefundedAt    *time.Time
	FailureReason string
	Metadata      map[string]any
}

// RefundPaymentInput is the service input for refunding a payment.
type RefundPaymentInput struct {
	StoreID       string
	IntegrationID string
	PaymentID     string
	Amount        *int64
}

// RefundPaymentOutput is the service output for refunding a payment.
type RefundPaymentOutput struct {
	RefundID  string
	Status    string
	Amount    int64
	CreatedAt time.Time
}

// ProcessCardPaymentInput is the service input for processing a card payment.
type ProcessCardPaymentInput struct {
	StoreID         string
	IntegrationID   string
	CartID          string
	CardToken       string
	Installments    int
	Customer        providers.CheckoutCustomer
	Items           []providers.CheckoutItem
	TotalAmount     int64
	Currency        string
	NotifyURL       string
	PaymentMethodID string
	IssuerID        string
	DeviceID        string
	Metadata        map[string]any
}

// ProcessCardPaymentOutput is the service output for processing a card payment.
type ProcessCardPaymentOutput struct {
	PaymentID         string
	Status            string
	StatusDetail      string
	Message           string
	Amount            int64
	Installments      int
	LastFourDigits    string
	CardBrand         string
	AuthorizationCode string
	PaidAt            *time.Time
}

// GeneratePixPaymentInput is the service input for generating a PIX payment.
type GeneratePixPaymentInput struct {
	StoreID       string
	IntegrationID string
	CartID        string
	Customer      providers.CheckoutCustomer
	Items         []providers.CheckoutItem
	TotalAmount   int64
	Currency      string
	NotifyURL     string
	Metadata      map[string]any
}

// GeneratePixPaymentOutput is the service output for generating a PIX payment.
type GeneratePixPaymentOutput struct {
	PaymentID  string
	QRCode     string
	QRCodeText string
	Amount     int64
	ExpiresAt  time.Time
	TicketURL  string
}
