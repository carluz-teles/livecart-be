package providers

import (
	"context"
	"time"
)

// ProviderType represents the category of integration.
type ProviderType string

const (
	ProviderTypePayment ProviderType = "payment"
	ProviderTypeERP     ProviderType = "erp"
	ProviderTypeSocial  ProviderType = "social"
)

// ProviderName represents a specific integration provider.
type ProviderName string

const (
	ProviderMercadoPago ProviderName = "mercado_pago"
	ProviderPagarme     ProviderName = "pagarme"
	ProviderTiny        ProviderName = "tiny"
	ProviderInstagram   ProviderName = "instagram"
)

// Credentials holds authentication data for providers.
// Stored encrypted in the database.
type Credentials struct {
	// OAuth2 credentials
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`

	// API Key credentials (for non-OAuth providers like Tiny)
	APIKey    string `json:"api_key,omitempty"`
	APISecret string `json:"api_secret,omitempty"`

	// Provider-specific extra data
	Extra map[string]any `json:"extra,omitempty"`
}

// IsExpired checks if OAuth credentials are expired or about to expire.
func (c *Credentials) IsExpired() bool {
	if c.ExpiresAt.IsZero() {
		return false // Non-expiring credentials
	}
	return time.Now().Add(5 * time.Minute).After(c.ExpiresAt)
}

// Provider is the base interface all providers must implement.
type Provider interface {
	// Type returns the provider type (payment, erp).
	Type() ProviderType

	// Name returns the provider name (mercado_pago, tiny).
	Name() ProviderName

	// ValidateCredentials checks if the current credentials are valid.
	ValidateCredentials(ctx context.Context) error

	// RefreshToken refreshes OAuth tokens if applicable.
	// Returns nil if the provider doesn't use OAuth or token refresh is not needed.
	RefreshToken(ctx context.Context) (*Credentials, error)

	// TestConnection tests the connection to the provider.
	// Returns detailed information about the connection status.
	TestConnection(ctx context.Context) (*TestConnectionResult, error)
}

// TestConnectionResult contains the result of a connection test.
type TestConnectionResult struct {
	Success     bool           `json:"success"`
	Message     string         `json:"message"`
	Latency     time.Duration  `json:"latency_ms"`
	AccountInfo map[string]any `json:"account_info,omitempty"` // Provider-specific account details
	TestedAt    time.Time      `json:"tested_at"`
}

// PaymentProvider interface for payment gateway integrations.
type PaymentProvider interface {
	Provider

	// CreateCheckout creates a payment checkout session.
	CreateCheckout(ctx context.Context, order CheckoutOrder) (*CheckoutResult, error)

	// GetPaymentStatus retrieves the current status of a payment.
	GetPaymentStatus(ctx context.Context, paymentID string) (*PaymentStatus, error)

	// RefundPayment initiates a refund for a payment.
	// If amount is nil, performs a full refund.
	RefundPayment(ctx context.Context, paymentID string, amount *int64) (*RefundResult, error)

	// ==========================================================================
	// TRANSPARENT CHECKOUT METHODS
	// ==========================================================================

	// GetPublicKey returns the public key for client-side SDK initialization.
	// For Mercado Pago: returns the public_key from OAuth credentials
	// For Pagar.me: returns the public_key stored in credentials
	GetPublicKey(ctx context.Context) (string, error)

	// ProcessCardPayment processes a payment with a tokenized card.
	// The card token is generated client-side using the payment provider's SDK.
	ProcessCardPayment(ctx context.Context, input CardPaymentInput) (*CardPaymentResult, error)

	// GeneratePixPayment generates a PIX QR code for payment.
	GeneratePixPayment(ctx context.Context, input PixPaymentInput) (*PixPaymentResult, error)

	// GetPaymentMethods returns the available payment methods for the store.
	GetPaymentMethods(ctx context.Context) ([]string, error)
}

// ERPProvider interface for ERP system integrations.
type ERPProvider interface {
	Provider

	// CreateOrder creates an order in the ERP for invoicing.
	CreateOrder(ctx context.Context, order ERPOrder) (*OrderResult, error)

	// LaunchOrderStock decrements stock in ERP for the order items.
	LaunchOrderStock(ctx context.Context, orderID string) error

	// ReverseOrderStock returns stock in ERP for the order items.
	ReverseOrderStock(ctx context.Context, orderID string) error

	// ApproveOrder sets the order status to approved in the ERP.
	ApproveOrder(ctx context.Context, orderID string) error

	// CancelOrder reverses stock and cancels an order in the ERP.
	CancelOrder(ctx context.Context, orderID string) error

	// ReserveStock creates a manual stock exit in the ERP. Returns movement ID.
	ReserveStock(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error)

	// ReverseStockReservation creates a manual stock entry in the ERP. Returns movement ID.
	ReverseStockReservation(ctx context.Context, productID string, qty int, unitPrice float64, obs string) (string, error)

	// SearchContacts searches for contacts by name or document.
	SearchContacts(ctx context.Context, params SearchContactsParams) ([]ERPContactResult, error)

	// CreateContact creates a new contact in the ERP.
	CreateContact(ctx context.Context, contact ERPContactInput) (*ERPContactResult, error)

	// ListProducts retrieves products from the ERP.
	ListProducts(ctx context.Context, params ListProductsParams) (*ProductListResult, error)

	// GetProduct retrieves a single product by ID.
	GetProduct(ctx context.Context, productID string) (*ERPProduct, error)

	// SyncProduct updates or creates a product in the ERP.
	SyncProduct(ctx context.Context, product ERPProduct) (*SyncResult, error)
}

// SocialProvider interface for social media integrations.
type SocialProvider interface {
	Provider

	// GetProfile retrieves the connected account profile information.
	GetProfile(ctx context.Context) (*SocialProfile, error)

	// SendDirectMessage sends a text DM to the given platform user.
	// Note: Subject to 24h messaging window restriction.
	SendDirectMessage(ctx context.Context, recipientID, text string) error

	// ReplyToComment replies to a comment (live or post) publicly.
	// This method does NOT have the 24h messaging window restriction.
	ReplyToComment(ctx context.Context, commentID, text string) error

	// SendPrivateReply sends a private DM to the user who made a comment.
	// This uses the Private Reply feature - sends a DM in response to a comment.
	// Unlike ReplyToComment (public), this sends a private message to the commenter.
	SendPrivateReply(ctx context.Context, commentID, text string) error

	// GetActiveLives retrieves all live videos currently being broadcast.
	// Only returns lives that are actively streaming at the time of the request.
	GetActiveLives(ctx context.Context) ([]LiveMedia, error)
}

// SocialProfile contains social media account information.
type SocialProfile struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
}

// LiveMedia represents a live video on a social platform.
type LiveMedia struct {
	ID               string `json:"id"`
	MediaType        string `json:"media_type"`
	MediaProductType string `json:"media_product_type"`
	Username         string `json:"username"`
	Timestamp        string `json:"timestamp,omitempty"`
}

// WebhookHandler interface for providers that support webhooks.
type WebhookHandler interface {
	// VerifySignature validates the webhook signature.
	VerifySignature(payload []byte, signature string, secret string) bool

	// ParseEvent parses the webhook payload into a typed event.
	ParseEvent(payload []byte) (*WebhookEvent, error)

	// HandleEvent processes a webhook event.
	HandleEvent(ctx context.Context, event *WebhookEvent) error
}

// =============================================================================
// PAYMENT TYPES
// =============================================================================

// CheckoutOrder represents an order to be paid.
type CheckoutOrder struct {
	ExternalID  string           `json:"external_id"`  // Your internal order/cart ID
	Items       []CheckoutItem   `json:"items"`
	Customer    CheckoutCustomer `json:"customer"`
	TotalAmount int64            `json:"total_amount"` // In cents
	Currency    string           `json:"currency"`     // BRL, USD, etc.
	NotifyURL   string           `json:"notify_url"`   // Webhook URL for payment notifications
	SuccessURL  string           `json:"success_url"`  // Redirect URL on success
	FailureURL  string           `json:"failure_url"`  // Redirect URL on failure
	ExpiresIn   *time.Duration   `json:"expires_in,omitempty"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
}

// CheckoutItem represents an item in the checkout.
type CheckoutItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Quantity    int    `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"` // In cents
	ImageURL    string `json:"image_url,omitempty"`
}

// CheckoutCustomer represents the customer paying.
type CheckoutCustomer struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Phone    string `json:"phone,omitempty"`
	Document string `json:"document,omitempty"` // CPF/CNPJ
}

// CheckoutResult is the result of creating a checkout.
type CheckoutResult struct {
	CheckoutID  string     `json:"checkout_id"`
	CheckoutURL string     `json:"checkout_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// PaymentStatus represents the current state of a payment.
type PaymentStatus struct {
	PaymentID         string         `json:"payment_id"`
	Status            PaymentState   `json:"status"`
	Amount            int64          `json:"amount"`
	PaidAt            *time.Time     `json:"paid_at,omitempty"`
	RefundedAt        *time.Time     `json:"refunded_at,omitempty"`
	FailureReason     string         `json:"failure_reason,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	ExternalReference string         `json:"external_reference,omitempty"` // Cart ID or order reference
	PaymentMethod     string         `json:"payment_method,omitempty"`     // pix, credit_card, debit_card, boleto
}

// PaymentState represents payment status values.
type PaymentState string

const (
	PaymentPending   PaymentState = "pending"
	PaymentApproved  PaymentState = "approved"
	PaymentRejected  PaymentState = "rejected"
	PaymentRefunded  PaymentState = "refunded"
	PaymentCancelled PaymentState = "cancelled"
	PaymentInProcess PaymentState = "in_process"
)

// RefundResult is the result of a refund operation.
type RefundResult struct {
	RefundID  string    `json:"refund_id"`
	Status    string    `json:"status"`
	Amount    int64     `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

// =============================================================================
// TRANSPARENT CHECKOUT TYPES
// =============================================================================

// CardPaymentInput contains the input for processing a card payment.
type CardPaymentInput struct {
	// CartID is the internal cart identifier
	CartID string `json:"cart_id"`

	// Token is the card token generated by the payment provider's SDK
	Token string `json:"token"`

	// Installments is the number of installments (1 for full payment)
	Installments int `json:"installments"`

	// Customer information
	Customer CheckoutCustomer `json:"customer"`

	// Items in the cart
	Items []CheckoutItem `json:"items"`

	// TotalAmount is the total payment amount in cents
	TotalAmount int64 `json:"total_amount"`

	// Currency code (BRL, USD, etc.)
	Currency string `json:"currency"`

	// NotifyURL is the webhook URL for payment notifications
	NotifyURL string `json:"notify_url"`

	// Metadata for additional context
	Metadata map[string]any `json:"metadata,omitempty"`

	// DeviceID for fraud prevention (optional, provider-specific)
	DeviceID string `json:"device_id,omitempty"`

	// PayerCost contains installment info from Mercado Pago SDK (optional)
	PayerCost *PayerCostInfo `json:"payer_cost,omitempty"`

	// PaymentMethodID for Mercado Pago (visa, master, etc.)
	PaymentMethodID string `json:"payment_method_id,omitempty"`

	// IssuerId for Mercado Pago
	IssuerID string `json:"issuer_id,omitempty"`
}

// PayerCostInfo contains installment cost information from Mercado Pago SDK.
type PayerCostInfo struct {
	Installments    int     `json:"installments"`
	InstallmentRate float64 `json:"installment_rate"`
	TotalAmount     float64 `json:"total_amount"`
}

// CardPaymentResult is the result of processing a card payment.
type CardPaymentResult struct {
	// PaymentID is the provider's payment identifier
	PaymentID string `json:"payment_id"`

	// Status is the payment status (approved, rejected, pending, in_process)
	Status PaymentState `json:"status"`

	// StatusDetail provides more info about the status (e.g., "accredited", "cc_rejected_other_reason")
	StatusDetail string `json:"status_detail,omitempty"`

	// Amount paid in cents
	Amount int64 `json:"amount"`

	// Installments used
	Installments int `json:"installments"`

	// Last four digits of the card
	LastFourDigits string `json:"last_four_digits,omitempty"`

	// Card brand (visa, master, etc.)
	CardBrand string `json:"card_brand,omitempty"`

	// ExternalReference is the cart ID or order reference
	ExternalReference string `json:"external_reference,omitempty"`

	// Message for the user
	Message string `json:"message,omitempty"`
}

// PixPaymentInput contains the input for generating a PIX payment.
type PixPaymentInput struct {
	// CartID is the internal cart identifier
	CartID string `json:"cart_id"`

	// Customer information
	Customer CheckoutCustomer `json:"customer"`

	// Items in the cart
	Items []CheckoutItem `json:"items"`

	// TotalAmount is the total payment amount in cents
	TotalAmount int64 `json:"total_amount"`

	// Currency code (BRL)
	Currency string `json:"currency"`

	// NotifyURL is the webhook URL for payment notifications
	NotifyURL string `json:"notify_url"`

	// ExpiresIn is how long the PIX code is valid (default: 30 minutes)
	ExpiresIn *time.Duration `json:"expires_in,omitempty"`

	// Metadata for additional context
	Metadata map[string]any `json:"metadata,omitempty"`
}

// PixPaymentResult is the result of generating a PIX payment.
type PixPaymentResult struct {
	// PaymentID is the provider's payment identifier
	PaymentID string `json:"payment_id"`

	// Status is the initial payment status (always pending for PIX)
	Status PaymentState `json:"status"`

	// QRCode is the PIX QR code as base64 image
	QRCode string `json:"qr_code"`

	// QRCodeText is the PIX copy-paste code (copia e cola)
	QRCodeText string `json:"qr_code_text"`

	// Amount in cents
	Amount int64 `json:"amount"`

	// ExpiresAt is when the PIX code expires
	ExpiresAt time.Time `json:"expires_at"`

	// ExternalReference is the cart ID or order reference
	ExternalReference string `json:"external_reference,omitempty"`

	// TicketURL is an optional URL to view the payment (Mercado Pago)
	TicketURL string `json:"ticket_url,omitempty"`
}

// CheckoutConfigResult contains the checkout configuration for the frontend.
type CheckoutConfigResult struct {
	// Provider name (mercado_pago, pagarme)
	Provider ProviderName `json:"provider"`

	// PublicKey for SDK initialization
	PublicKey string `json:"public_key"`

	// AvailableMethods lists the payment methods available (card, pix)
	AvailableMethods []string `json:"available_methods"`
}

// =============================================================================
// ERP TYPES
// =============================================================================

// ERPOrder represents an order to create in the ERP.
type ERPOrder struct {
	ExternalID  string         `json:"external_id"`            // Your internal order/cart ID
	ContactID   string         `json:"contact_id"`             // ERP contact ID (required for Tiny v3)
	Items       []ERPOrderItem `json:"items"`
	TotalAmount int64          `json:"total_amount"`           // In cents
	Observation string         `json:"observation,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// SearchContactsParams contains parameters for searching contacts.
type SearchContactsParams struct {
	Name    string `json:"name,omitempty"`
	CpfCnpj string `json:"cpf_cnpj,omitempty"`
}

// ERPContactInput represents data for creating a contact in the ERP.
type ERPContactInput struct {
	Name       string `json:"name"`
	CpfCnpj    string `json:"cpf_cnpj,omitempty"`
	Email      string `json:"email,omitempty"`
	Phone      string `json:"phone,omitempty"`
	PersonType string `json:"person_type,omitempty"` // "F" = Física, "J" = Jurídica
}

// ERPContactResult is the result of searching or creating a contact.
type ERPContactResult struct {
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// ERPOrderItem represents an item in an ERP order.
type ERPOrderItem struct {
	ProductID   string `json:"product_id"` // ERP product ID
	SKU         string `json:"sku,omitempty"`
	Name        string `json:"name"`
	Quantity    int    `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"` // In cents
}

// OrderResult is the result of creating an order in the ERP.
type OrderResult struct {
	OrderID     string `json:"order_id"`     // ERP order ID
	OrderNumber string `json:"order_number"` // Human-readable order number
	Status      string `json:"status"`
}

// ListProductsParams contains parameters for listing products.
type ListProductsParams struct {
	Page         int        `json:"page,omitempty"`
	PageSize     int        `json:"page_size,omitempty"`
	Search       string     `json:"search,omitempty"`
	GTIN         string     `json:"gtin,omitempty"`
	SKU          string     `json:"sku,omitempty"`
	ActiveOnly   bool       `json:"active_only,omitempty"`
	UpdatedAfter *time.Time `json:"updated_after,omitempty"`
}

// ProductListResult contains the result of listing products.
type ProductListResult struct {
	Products   []ERPProduct `json:"products"`
	TotalCount int          `json:"total_count"`
	Page       int          `json:"page"`
	PageSize   int          `json:"page_size"`
	HasMore    bool         `json:"has_more"`
}

// ERPProduct represents a product in the ERP.
type ERPProduct struct {
	ID          string    `json:"id"`           // ERP product ID
	SKU         string    `json:"sku,omitempty"`
	GTIN        string    `json:"gtin,omitempty"` // Barcode (EAN/GTIN)
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Price       int64     `json:"price"`        // In cents
	Stock       int       `json:"stock"`
	Active      bool      `json:"active"`
	ImageURL    string    `json:"image_url,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SyncResult is the result of syncing a product.
type SyncResult struct {
	ProductID string `json:"product_id"`
	Action    string `json:"action"` // created, updated, skipped
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// =============================================================================
// WEBHOOK TYPES
// =============================================================================

// WebhookEvent represents a parsed webhook event.
type WebhookEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Action    string         `json:"action,omitempty"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
}
