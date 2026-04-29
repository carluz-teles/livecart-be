package integration

import (
	"context"
	"time"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/query"
)

// =============================================================================
// NOTIFIER INTERFACE (stub for future notification implementation)
// =============================================================================

// Notifier sends notifications to platform users (e.g., DM on Instagram).
type Notifier interface {
	// NotifyWaitlistAvailable notifies a user that a waitlisted product is now available.
	NotifyWaitlistAvailable(ctx context.Context, params NotifyWaitlistParams) error

	// NotifyCartExpiring notifies a user that their cart is about to expire.
	NotifyCartExpiring(ctx context.Context, params NotifyCartExpiringParams) error

	// NotifyEventCheckout notifies a user that the live event ended and their cart
	// is ready for checkout.
	NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) error
}

// NotifyWaitlistParams holds data for waitlist notifications.
type NotifyWaitlistParams struct {
	PlatformUserID string
	PlatformHandle string
	ProductName    string
	ProductKeyword string
	ClaimMinutes   int
}

// NotifyCartExpiringParams holds data for cart expiration notifications.
type NotifyCartExpiringParams struct {
	PlatformUserID string
	PlatformHandle string
	CartID         string
	ExpiresInMin   int
}

// NotifyEventCheckoutParams holds data for end-of-event checkout notifications.
type NotifyEventCheckoutParams struct {
	StoreID        string
	EventID        string
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	TotalItems     int
	TotalValue     int64 // cents
}

// NoopNotifier is a placeholder that does nothing. Replace with real implementation later.
type NoopNotifier struct{}

func (n *NoopNotifier) NotifyWaitlistAvailable(_ context.Context, _ NotifyWaitlistParams) error {
	return nil
}
func (n *NoopNotifier) NotifyCartExpiring(_ context.Context, _ NotifyCartExpiringParams) error {
	return nil
}
func (n *NoopNotifier) NotifyEventCheckout(_ context.Context, _ NotifyEventCheckoutParams) error {
	return nil
}

// =============================================================================
// REQUEST/RESPONSE TYPES (HTTP Layer)
// =============================================================================

// CreateIntegrationRequest is the HTTP request body for creating an integration.
//
// The `Type` and `Provider` oneof lists must stay in sync with the DB check
// constraints (`integrations_type_check`, `integrations_provider_check`) and
// with the factory switches in providers/factory.go — whenever a new provider
// is plugged in, add it here too or this generic endpoint will 422.
type CreateIntegrationRequest struct {
	Type        string         `json:"type" validate:"required,oneof=payment erp social shipping"`
	Provider    string         `json:"provider" validate:"required,oneof=mercado_pago pagarme tiny instagram melhor_envio smartenvios"`
	Credentials map[string]any `json:"credentials" validate:"required"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// UpdateIntegrationRequest is the HTTP request body for updating an integration.
type UpdateIntegrationRequest struct {
	Credentials map[string]any `json:"credentials,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Status      string         `json:"status,omitempty" validate:"omitempty,oneof=active error disconnected"`
}

// IntegrationResponse is the HTTP response for an integration.
type IntegrationResponse struct {
	ID                string         `json:"id"`
	StoreID           string         `json:"storeId"`
	Type              string         `json:"type"`
	Provider          string         `json:"provider"`
	Status            string         `json:"status"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	LastSyncedAt      *time.Time     `json:"lastSyncedAt,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	// Setup URLs the merchant must paste into the provider's app config.
	// Populated for providers that need user-side configuration (e.g. Tiny ERP).
	RedirectURL       string         `json:"redirectUrl,omitempty"`
	WebhookURL        string         `json:"webhookUrl,omitempty"`
	// WebhookLastPingAt is the last time this provider hit our webhook URL for
	// this store (validation ping or real event). nil = never received → URL is
	// likely missing or wrong on the provider side.
	WebhookLastPingAt *time.Time     `json:"webhookLastPingAt,omitempty"`
}

// ProviderURLsResponse exposes the setup URLs a merchant must paste into the
// provider's app config (e.g. Tiny redirect URL + webhook URL). Returned by
// GET /integrations/providers/:provider/urls so the frontend can display them
// before/while creating the integration.
type ProviderURLsResponse struct {
	Provider    string `json:"provider"`
	RedirectURL string `json:"redirectUrl,omitempty"`
	WebhookURL  string `json:"webhookUrl,omitempty"`
}

// ListIntegrationsResponse is the HTTP response for listing integrations.
type ListIntegrationsResponse struct {
	Data       []IntegrationResponse  `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// CreateCheckoutRequest is the HTTP request for creating a payment checkout.
type CreateCheckoutRequest struct {
	IntegrationID string                    `json:"integrationId" validate:"required,uuid"`
	CartID        string                    `json:"cartId" validate:"required"`
	Items         []providers.CheckoutItem  `json:"items" validate:"required,min=1,dive"`
	Customer      providers.CheckoutCustomer `json:"customer" validate:"required"`
	TotalAmount   int64                      `json:"totalAmount" validate:"required,gt=0"`
	Currency      string                     `json:"currency" validate:"required,len=3"`
	SuccessURL    string                     `json:"successUrl" validate:"required,url"`
	FailureURL    string                     `json:"failureUrl" validate:"required,url"`
	Metadata      map[string]any             `json:"metadata,omitempty"`
}

// CheckoutResponse is the HTTP response for a checkout.
type CheckoutResponse struct {
	CheckoutID  string     `json:"checkoutId"`
	CheckoutURL string     `json:"checkoutUrl"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
}

// PaymentStatusResponse is the HTTP response for payment status.
type PaymentStatusResponse struct {
	PaymentID     string         `json:"paymentId"`
	Status        string         `json:"status"`
	Amount        int64          `json:"amount"`
	PaidAt        *time.Time     `json:"paidAt,omitempty"`
	RefundedAt    *time.Time     `json:"refundedAt,omitempty"`
	FailureReason string         `json:"failureReason,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// RefundRequest is the HTTP request for refunding a payment.
type RefundRequest struct {
	IntegrationID string `json:"integrationId" validate:"required,uuid"`
	PaymentID     string `json:"paymentId" validate:"required"`
	Amount        *int64 `json:"amount,omitempty"` // nil = full refund
}

// RefundResponse is the HTTP response for a refund.
type RefundResponse struct {
	RefundID  string    `json:"refundId"`
	Status    string    `json:"status"`
	Amount    int64     `json:"amount"`
	CreatedAt time.Time `json:"createdAt"`
}

// OAuthConnectResponse is the HTTP response for initiating OAuth.
type OAuthConnectResponse struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}

// TestConnectionResponse is the HTTP response for testing a connection.
type TestConnectionResponse struct {
	Success     bool           `json:"success"`
	Message     string         `json:"message"`
	LatencyMs   int64          `json:"latencyMs"`
	AccountInfo map[string]any `json:"accountInfo,omitempty"`
	TestedAt    time.Time      `json:"testedAt"`
}

// =============================================================================
// INPUT/OUTPUT TYPES (Service Layer)
// =============================================================================

// ListIntegrationsInput is the service input for listing integrations.
type ListIntegrationsInput struct {
	StoreID    string
	Pagination query.Pagination
}

// ListIntegrationsOutput is the service output for listing integrations.
type ListIntegrationsOutput struct {
	Integrations []CreateIntegrationOutput
	Pagination   query.Pagination
	Total        int
}

// CreateIntegrationInput is the service input for creating an integration.
type CreateIntegrationInput struct {
	StoreID     string
	Type        string
	Provider    string
	Credentials *providers.Credentials
	Metadata    map[string]any
}

// CreateIntegrationOutput is the service output for creating an integration.
type CreateIntegrationOutput struct {
	ID                string
	StoreID           string
	Type              string
	Provider          string
	Status            string
	Metadata          map[string]any
	LastSyncedAt      *time.Time
	CreatedAt         time.Time
	RedirectURL       string
	WebhookURL        string
	WebhookLastPingAt *time.Time
}

// GetProviderURLsInput is the service input for resolving the per-provider
// redirect + webhook URLs the merchant must paste into the provider's app config.
type GetProviderURLsInput struct {
	StoreID  string
	Provider string
}

// GetProviderURLsOutput is the service output for GetProviderURLs.
type GetProviderURLsOutput struct {
	Provider    string
	RedirectURL string
	WebhookURL  string
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

// GetOAuthURLInput is the service input for getting OAuth URL.
type GetOAuthURLInput struct {
	StoreID  string
	Provider string
}

// GetOAuthURLOutput is the service output for getting OAuth URL.
type GetOAuthURLOutput struct {
	AuthURL string
	State   string
}

// OAuthCallbackInput is the input for handling OAuth callback.
type OAuthCallbackInput struct {
	Provider string
	Code     string
	State    string
}

// OAuthCallbackOutput is the output for handling OAuth callback.
type OAuthCallbackOutput struct {
	IntegrationID string
	StoreID       string
	Provider      string
	Status        string
}

// SearchProductsInput is the service input for searching products in an ERP.
type SearchProductsInput struct {
	StoreID       string
	IntegrationID string
	Search        string
	PageSize      int
}

// SearchProductsOutput is the service output for searching products.
type SearchProductsOutput struct {
	Products   []ERPProductResponse `json:"products"`
	TotalCount int                  `json:"totalCount"`
	HasMore    bool                 `json:"hasMore"`
}

// ERPProductResponse is the HTTP response for an ERP product.
// When IsParent is true, Stock is the SUM of variant stocks (the parent itself
// holds no stock in Tiny/Bling); the front-end must use Variants to let the
// user pick a specific SKU before adding to a cart/live.
type ERPProductResponse struct {
	ID          string                  `json:"id"`
	SKU         string                  `json:"sku,omitempty"`
	GTIN        string                  `json:"gtin,omitempty"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Price       int64                   `json:"price"`
	Stock       int                     `json:"stock"`
	ImageURL    string                  `json:"imageUrl,omitempty"`
	Active      bool                    `json:"active"`
	Shipping    *ERPShippingPreviewDTO  `json:"shipping,omitempty"` // weight + dimensions resolved from the ERP, for the picker preview
	IsParent    bool                    `json:"isParent,omitempty"`
	Variants    []ERPVariantResponse    `json:"variants,omitempty"`
}

// ERPShippingPreviewDTO mirrors the ERP-side shipping data the search response
// surfaces so the front can show "peso/dimensões a importar" before the user
// confirms.
type ERPShippingPreviewDTO struct {
	WeightGrams   int    `json:"weightGrams,omitempty"`
	HeightCm      int    `json:"heightCm,omitempty"`
	WidthCm       int    `json:"widthCm,omitempty"`
	LengthCm      int    `json:"lengthCm,omitempty"`
	PackageFormat string `json:"packageFormat,omitempty"`
}

// shippingPreviewFromERP builds the picker-side preview DTO. When Shipping is
// fully resolved we expose all four fields plus format. When only weight is
// known (WeightGramsHint > 0 but no usable dimensions), we still surface the
// weight so the front can warn "Faltam dimensões — store defaults serão
// aplicados". Returns nil when nothing is known.
func shippingPreviewFromERP(s *providers.ERPShippingProfile, weightHint int) *ERPShippingPreviewDTO {
	if s != nil {
		return &ERPShippingPreviewDTO{
			WeightGrams:   s.WeightGrams,
			HeightCm:      s.HeightCm,
			WidthCm:       s.WidthCm,
			LengthCm:      s.LengthCm,
			PackageFormat: s.PackageFormat,
		}
	}
	if weightHint > 0 {
		return &ERPShippingPreviewDTO{WeightGrams: weightHint}
	}
	return nil
}

// ERPVariantResponse is one child SKU of an ERP product with variations.
type ERPVariantResponse struct {
	ID         string                 `json:"id"`
	SKU        string                 `json:"sku,omitempty"`
	GTIN       string                 `json:"gtin,omitempty"`
	Name       string                 `json:"name,omitempty"`
	Price      int64                  `json:"price"`
	Stock      int                    `json:"stock"`
	Active     bool                   `json:"active"`
	ImageURL   string                 `json:"imageUrl,omitempty"` // best-effort enrichment from GetProduct(child); may be empty if Tiny returned no anexos or the enrichment timed out — front should fall back to parent.imageUrl
	Shipping   *ERPShippingPreviewDTO `json:"shipping,omitempty"` // resolved per-variant shipping (after individual GET enrichment)
	Attributes map[string]string      `json:"attributes,omitempty"` // e.g. {"Cor":"Azul","Tamanho":"M"}
}

// SyncProductInput is the service input for manually syncing a product from an ERP.
type SyncProductInput struct {
	StoreID       string
	IntegrationID string
	ProductID     string
}

// ImportERPProductInput is the service input for importing a product (and
// optionally a subset of its variations) from the ERP into the LiveCart
// catalog. When VariantIDs is empty the entire product (all variations, or the
// single SKU when not a parent) is imported.
type ImportERPProductInput struct {
	StoreID        string
	IntegrationID  string
	TinyProductID  string
	VariantIDs     []string
}

// ImportERPProductRequest is the HTTP body of POST /integrations/:id/products/:tinyProductId/import.
type ImportERPProductRequest struct {
	VariantIDs []string `json:"variantIds" validate:"omitempty,dive,required"`
}

// ImportERPProductOutput is the HTTP response after a successful ERP import.
// GroupID is populated when the imported product had variations; ProductID is
// populated when a single simple product was imported.
type ImportERPProductOutput struct {
	GroupID    string                       `json:"groupId,omitempty"`
	ProductID  string                       `json:"productId,omitempty"`
	IsParent   bool                         `json:"isParent"`
	Imported   []ImportedERPVariantSummary  `json:"imported"`
}

type ImportedERPVariantSummary struct {
	ExternalID string            `json:"externalId"`           // Tiny child product id
	SKU        string            `json:"sku,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// SyncProductOutput is the service output for a manual product sync.
type SyncProductOutput struct {
	ProductID  string `json:"productId"`
	ExternalID string `json:"externalId"`
	Name       string `json:"name"`
	Price      int64  `json:"price"`
	Stock      int    `json:"stock"`
	ImageURL   string `json:"imageUrl"`
	Active     bool   `json:"active"`
}

// TestConnectionInput is the service input for testing a connection.
type TestConnectionInput struct {
	StoreID       string
	IntegrationID string
}

// TestConnectionOutput is the service output for testing a connection.
type TestConnectionOutput struct {
	Success     bool
	Message     string
	Latency     time.Duration
	AccountInfo map[string]any
	TestedAt    time.Time
}

// =============================================================================
// TRANSPARENT CHECKOUT INPUT/OUTPUT (Service Layer)
// =============================================================================

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
	PaymentID      string
	Status         string
	StatusDetail   string
	Message        string
	Amount         int64
	Installments   int
	LastFourDigits string
	CardBrand      string
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

// =============================================================================
// REPOSITORY TYPES (Data Layer)
// =============================================================================

// IntegrationRow represents a row from the integrations table.
type IntegrationRow struct {
	ID             string
	StoreID        string
	Type           string
	Provider       string
	Status         string
	Credentials    []byte // Encrypted
	TokenExpiresAt *time.Time
	Metadata       map[string]any
	LastSyncedAt   *time.Time
	CreatedAt      time.Time
}

// CreateIntegrationParams contains parameters for creating an integration.
type CreateIntegrationParams struct {
	StoreID        string
	Type           string
	Provider       string
	Status         string
	Credentials    []byte
	TokenExpiresAt *time.Time
	Metadata       map[string]any
}

// UpdateIntegrationParams contains parameters for updating an integration.
type UpdateIntegrationParams struct {
	ID             string
	Credentials    []byte
	TokenExpiresAt *time.Time
	Status         string
}

// =============================================================================
// WEBHOOK TYPES
// =============================================================================

// StoreWebhookInput is the input for storing a webhook event.
type StoreWebhookInput struct {
	StoreID        string // From URL parameter
	Provider       string
	IntegrationID  string // Resolved by service layer before storing
	EventType      string
	EventID        string
	Payload        []byte
	SignatureValid bool
}

// ProcessPaymentInput is the input for processing a payment notification.
type ProcessPaymentInput struct {
	StoreID   string
	Provider  string
	PaymentID string
}

// WebhookEventRow represents a row from the webhook_events table.
type WebhookEventRow struct {
	ID            string
	IntegrationID string
	Provider      string
	EventType     string
	EventID       string
	Payload       []byte
	SignatureValid *bool
	Processed     bool
	ProcessedAt   *time.Time
	ErrorMessage  string
	CreatedAt     time.Time
}
