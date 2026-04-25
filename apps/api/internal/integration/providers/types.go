package providers

import (
	"context"
	"errors"
	"time"
)

// ErrOperationNotSupported is returned by providers that do not implement a
// specific capability (for example, a carrier aggregator that only quotes but
// cannot create shipments). Callers should match against this sentinel rather
// than parsing error strings.
var ErrOperationNotSupported = errors.New("operation not supported by this provider")

// ProviderType represents the category of integration.
type ProviderType string

const (
	ProviderTypePayment  ProviderType = "payment"
	ProviderTypeERP      ProviderType = "erp"
	ProviderTypeSocial   ProviderType = "social"
	ProviderTypeShipping ProviderType = "shipping"
)

// ProviderName represents a specific integration provider.
type ProviderName string

const (
	ProviderMercadoPago ProviderName = "mercado_pago"
	ProviderPagarme     ProviderName = "pagarme"
	ProviderTiny        ProviderName = "tiny"
	ProviderInstagram   ProviderName = "instagram"
	ProviderMelhorEnvio ProviderName = "melhor_envio"
	ProviderSmartEnvios ProviderName = "smartenvios"
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
	Installments      int            `json:"installments,omitempty"`
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
	TotalAmount int64          `json:"total_amount"`           // In cents (includes shipping when present)
	Observation string         `json:"observation,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`

	// ShippingAddress is the delivery address. When set, the provider ships it
	// as enderecoEntrega (or equivalent) on the order.
	ShippingAddress *ERPShippingAddress `json:"shipping_address,omitempty"`

	// Shipping, when set, feeds the provider with the carrier/service chosen
	// by the customer and the freight value so the ERP records the shipment.
	Shipping *ERPOrderShipping `json:"shipping,omitempty"`

	// Payment, when set, flags the order as already paid: the provider fills
	// parcelas with dataPagamento, records the payment method/ID and approves
	// the order in the ERP.
	Payment *ERPOrderPayment `json:"payment,omitempty"`
}

// ERPOrderShipping captures the freight option chosen at checkout so the ERP
// records the shipment alongside the sales order.
type ERPOrderShipping struct {
	Carrier      string `json:"carrier"`                 // "Correios", "Jadlog"...
	Service      string `json:"service"`                 // "PAC", "SEDEX", ".Package"...
	CostCents    int64  `json:"cost_cents"`              // actual merchant cost (real quote value)
	DeadlineDays int    `json:"deadline_days,omitempty"` // estimated max delivery time
}

// ERPShippingAddress describes a delivery address for an ERP order.
type ERPShippingAddress struct {
	RecipientName string `json:"recipient_name,omitempty"` // nomeDestinatario
	Document      string `json:"document,omitempty"`       // cpfCnpj
	Street        string `json:"street"`                   // endereco
	Number        string `json:"number"`                   // enderecoNro
	Complement    string `json:"complement,omitempty"`     // complemento
	Neighborhood  string `json:"neighborhood"`             // bairro
	City          string `json:"city"`                     // municipio
	State         string `json:"state"`                    // uf (2 chars)
	ZipCode       string `json:"zip_code"`                 // cep
	Phone         string `json:"phone,omitempty"`          // fone
}

// ERPOrderPayment captures the payment confirmation details so the provider
// can register the order as paid (e.g. Tiny parcelas with dataPagamento).
type ERPOrderPayment struct {
	Method       string    `json:"method"`       // pix, credit_card, debit_card, boleto
	PaymentID    string    `json:"payment_id"`   // gateway payment ID
	Installments int       `json:"installments,omitempty"`
	PaidAt       time.Time `json:"paid_at"`
	Amount       int64     `json:"amount"`       // paid amount, in cents (usually == TotalAmount)
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
	ID          string    `json:"id"`             // ERP product ID
	SKU         string    `json:"sku,omitempty"`
	GTIN        string    `json:"gtin,omitempty"` // Barcode (EAN/GTIN)
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Price       int64     `json:"price"` // In cents
	Stock       int       `json:"stock"`
	Active      bool      `json:"active"`
	ImageURL    string    `json:"image_url,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Variant-related fields (populated for ERPs that expose variations like Tiny "Com Variações" / tipo=V).
	// Type carries the ERP's native product type ("S","V","K","F","M" for Tiny). Empty when unknown.
	Type             string            `json:"type,omitempty"`
	IsParent         bool              `json:"is_parent,omitempty"`         // True when this product is the aggregator (has children).
	ParentExternalID string            `json:"parent_external_id,omitempty"` // ERP id of the parent when this is a child variant.
	Attributes       map[string]string `json:"attributes,omitempty"`        // Variation grade for a child, e.g. {"Cor":"Azul","Tamanho":"M"}.
	GradeKeys        []string          `json:"grade_keys,omitempty"`        // Grade dimension names for a parent, e.g. ["Tamanho","Cor"].
	Variants         []ERPProduct      `json:"variants,omitempty"`          // Children when IsParent — populated by GetProduct.
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

// =============================================================================
// SHIPPING TYPES
// =============================================================================

// ShippingProvider is the minimum contract all shipping integrations must
// implement. It covers quoting (checkout) and carrier listing (admin / test
// connection). Order-lifecycle operations (create shipment, invoice, labels,
// tracking) live on the optional ShippingOrderProvider extension so that
// quote-only aggregators are still valid providers.
type ShippingProvider interface {
	Provider

	// Quote calculates freight options for a shipment.
	Quote(ctx context.Context, req QuoteRequest) ([]QuoteOption, error)

	// ListCarriers returns the available carriers/services for the connected account.
	ListCarriers(ctx context.Context) ([]CarrierService, error)
}

// ShippingOrderProvider extends ShippingProvider with post-quote operations.
// Providers that implement it can: create shipments, attach/upload invoices,
// generate labels, and pull tracking history. Callers should type-assert and
// surface ErrOperationNotSupported when the provider does not implement this
// interface or when a specific call returns it.
type ShippingOrderProvider interface {
	ShippingProvider

	// CreateShipment creates a freight order at the carrier aggregator, optionally
	// tied to a prior quote (QuoteServiceID). Returns the provider's shipment
	// reference that the caller should persist.
	CreateShipment(ctx context.Context, req CreateShipmentRequest) (*CreateShipmentResult, error)

	// AttachInvoice links an already-emitted fiscal document (NFe/DCe) to an
	// existing shipment by key. Use this for async flows where the invoice is
	// emitted after the shipment is created.
	AttachInvoice(ctx context.Context, req AttachInvoiceRequest) error

	// UploadInvoiceXML uploads the XML of the fiscal document to the carrier
	// aggregator. Required when the aggregator cannot fetch the XML from the
	// SEFAZ by key alone.
	UploadInvoiceXML(ctx context.Context, req UploadInvoiceXMLRequest) error

	// GenerateLabels produces the shipping labels (PDF/ZPL/base64) for the
	// given shipments. Result contains the downloadable URL plus per-volume
	// barcodes.
	GenerateLabels(ctx context.Context, req GenerateLabelsRequest) (*GenerateLabelsResult, error)

	// TrackShipment pulls the latest tracking history for a shipment. Use as
	// fallback when webhooks are not wired up.
	TrackShipment(ctx context.Context, req TrackShipmentRequest) (*TrackShipmentResult, error)
}

// ShippingZip is a Brazilian CEP, digits only (8 chars).
type ShippingZip string

// ShippingItem represents a cart item being quoted.
type ShippingItem struct {
	ID                  string // opaque identifier returned in error messages
	Name                string // human-readable description (used when creating shipments)
	WeightGrams         int
	HeightCm            int
	WidthCm             int
	LengthCm            int
	InsuranceValueCents int64
	UnitPriceCents      int64 // unit price (used when creating shipments)
	Quantity            int
	PackageFormat       string // "box", "roll", "letter" - optional, carrier hint
}

// QuoteRequest is the input for a freight quote.
type QuoteRequest struct {
	FromZip ShippingZip
	ToZip   ShippingZip
	Items   []ShippingItem
	// ExtraPackageWeightGrams is added once to the shipment to account for
	// consolidating packaging (empty box, bubble wrap). Applied to the heaviest
	// item when the provider quotes by individual products.
	ExtraPackageWeightGrams int
	// ServiceIDs restricts the quote to a subset of services. Empty = all.
	// Opaque strings because providers use different id formats (int, UUID,
	// MongoDB ObjectId, ...).
	ServiceIDs []string
	// ExternalID is an optional caller-side correlation id (e.g. cart id)
	// forwarded to providers that support it for correlation with webhooks.
	ExternalID string
	// Options are delivery-time flags.
	Receipt bool
	OwnHand bool
}

// QuoteOption is a single carrier/service result.
type QuoteOption struct {
	// Provider is the integration name that returned this option. Required so
	// the caller can route the follow-up CreateShipment call to the right
	// provider when the store has multiple shipping integrations active.
	Provider ProviderName

	// ServiceID is the opaque, provider-specific identifier for the service.
	// Pass it back as-is when creating the shipment.
	ServiceID    string
	Service      string // "PAC", "SEDEX", ".Package", etc.
	Carrier      string // "Correios", "Jadlog", "Loggi", etc.
	CarrierLogo  string // optional URL
	PriceCents   int64  // final price in cents
	DeadlineDays int    // business days
	Available    bool
	Error        string // populated when Available is false
}

// CarrierService describes one service offered by a carrier.
type CarrierService struct {
	ServiceID   string
	Service     string
	Carrier     string
	CarrierLogo string
	// Max insurance value accepted, in cents. 0 means unlimited/unknown.
	InsuranceMaxCents int64
}

// =============================================================================
// SHIPPING ORDER LIFECYCLE TYPES
// =============================================================================

// ShippingAddress describes an address used by CreateShipment (sender/destiny).
type ShippingAddressPoint struct {
	Name         string
	Document     string // CPF/CNPJ
	ZipCode      string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string // 2-letter UF
	Phone        string
	Email        string
	Observation  string
}

// CreateShipmentRequest captures everything a provider needs to turn a quote
// into a concrete freight order.
type CreateShipmentRequest struct {
	// QuoteServiceID is the opaque id returned by Quote() for the chosen
	// carrier/service. Required — callers must not create shipments without
	// a prior quote (no auto-selection in LiveCart).
	QuoteServiceID string

	// ExternalOrderID is the caller's own order identifier (used for webhook
	// correlation and lookups).
	ExternalOrderID string

	// InvoiceKey, when present, is the NFe access key. When absent the
	// shipment is created as a Declaração de Conteúdo and the invoice can be
	// linked later via AttachInvoice / UploadInvoiceXML.
	InvoiceKey string

	Sender  ShippingAddressPoint
	Destiny ShippingAddressPoint

	// Items in the shipment. Dimensions/weight MUST be set.
	Items []ShippingItem

	// VolumeCount is the number of physical packages in the shipment.
	VolumeCount int

	// Observation is free-form text appended to the shipment record.
	Observation string
}

// CreateShipmentResult is the normalized response after creating a shipment.
type CreateShipmentResult struct {
	ProviderOrderID     string // provider's internal order id (persisted)
	ProviderOrderNumber string // human-readable order number (optional)
	TrackingCode        string // provider tracking code
	InvoiceID           string // provider's id for the linked NFe (optional)
	Status              TrackingStatus
	StatusRawCode       int
	StatusRawName       string
	CreatedAt           time.Time
	// ProviderMeta is the raw response for debugging / auditing. Persisted as JSONB.
	ProviderMeta map[string]any
}

// AttachInvoiceRequest links an already-emitted NFe/DCe to an existing shipment.
type AttachInvoiceRequest struct {
	ProviderOrderID string
	ExternalOrderID string // some providers identify the order by external id
	InvoiceKey      string // NFe or DCe key (44 chars for NFe)
	InvoiceKind     string // "nfe" | "dce"
}

// UploadInvoiceXMLRequest uploads the full NFe XML file.
type UploadInvoiceXMLRequest struct {
	ProviderOrderID string
	ExternalOrderID string
	XML             []byte
	Filename        string // "nfe-12345.xml"
}

// GenerateLabelsRequest identifies which shipments should have labels generated.
// Providers accept multiple identifier types; the caller fills whichever it has.
type GenerateLabelsRequest struct {
	ProviderOrderIDs []string
	TrackingCodes    []string
	InvoiceKeys      []string
	ExternalOrderIDs []string

	// Format is the preferred label format. Providers may ignore unsupported
	// values. Known: "pdf", "zpl", "base64".
	Format string

	// DocumentType controls how the label interacts with the DANFE — when
	// supported by the provider. Known: "label_integrated_danfe", "label_separate_danfe".
	DocumentType string
}

// GenerateLabelsResult contains the URL of the label batch plus per-shipment
// tickets. Shape is normalized across providers.
type GenerateLabelsResult struct {
	LabelURL string
	Tickets  []LabelTicket
}

// LabelTicket represents the labels for a single shipment.
type LabelTicket struct {
	ProviderOrderID string
	TrackingCode    string
	PublicTracking  string   // public URL the customer can check
	VolumeBarcodes  []string // one barcode per physical package
}

// TrackShipmentRequest identifies which shipment to pull tracking for.
// Exactly ONE field should be set.
type TrackShipmentRequest struct {
	ProviderOrderID string
	ExternalOrderID string
	InvoiceKey      string
	TrackingCode    string
}

// TrackShipmentResult contains the normalized tracking history.
type TrackShipmentResult struct {
	TrackingCode    string
	Carrier         string
	Service         string
	CurrentStatus   TrackingStatus
	Events          []TrackingEvent
	ProviderMeta    map[string]any
}

// TrackingEvent is a single movement in the tracking history.
type TrackingEvent struct {
	Status      TrackingStatus
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
}

// TrackingStatus is the LiveCart-normalized shipment status. Every provider
// translates its own status codes into this enum so downstream consumers
// (admin UI, notifications, reports) are provider-agnostic.
type TrackingStatus string

const (
	TrackingStatusUnknown                   TrackingStatus = "unknown"
	TrackingStatusAwaitingInvoice           TrackingStatus = "awaiting_invoice"
	TrackingStatusPending                   TrackingStatus = "pending"
	TrackingStatusPendingPickup             TrackingStatus = "pending_pickup"
	TrackingStatusPendingDropoff            TrackingStatus = "pending_dropoff"
	TrackingStatusAwaitingPickup            TrackingStatus = "awaiting_pickup"
	TrackingStatusInTransit                 TrackingStatus = "in_transit"
	TrackingStatusOutForDelivery            TrackingStatus = "out_for_delivery"
	TrackingStatusDelivered                 TrackingStatus = "delivered"
	TrackingStatusDeliveryIssue             TrackingStatus = "delivery_issue"
	TrackingStatusDeliveryBlocked           TrackingStatus = "delivery_blocked"
	TrackingStatusIssue                     TrackingStatus = "issue"
	TrackingStatusShipmentBlocked           TrackingStatus = "shipment_blocked"
	TrackingStatusDamaged                   TrackingStatus = "damaged"
	TrackingStatusStolen                    TrackingStatus = "stolen"
	TrackingStatusLost                      TrackingStatus = "lost"
	TrackingStatusFiscalIssue               TrackingStatus = "fiscal_issue"
	TrackingStatusRefused                   TrackingStatus = "refused"
	TrackingStatusNotDelivered              TrackingStatus = "not_delivered"
	TrackingStatusIndemnificationRequested  TrackingStatus = "indemnification_requested"
	TrackingStatusIndemnificationScheduled  TrackingStatus = "indemnification_scheduled"
	TrackingStatusIndemnificationCompleted  TrackingStatus = "indemnification_completed"
	TrackingStatusReturning                 TrackingStatus = "returning"
	TrackingStatusReturned                  TrackingStatus = "returned"
	TrackingStatusCanceled                  TrackingStatus = "canceled"
)
