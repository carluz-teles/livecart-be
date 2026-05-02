package checkout

import (
	"time"
)

// =============================================================================
// REQUEST/RESPONSE DTOs (Handler layer)
// =============================================================================

// CartForCheckoutResponse is the response for GET /api/public/checkout/:token
//
// Customer / ShippingAddress / Payment are only populated when PaymentStatus
// == "paid" so the post-payment "comprovante" page can render the buyer's
// data without leaking PII while the checkout token is still pre-payment.
// All three are optional on the wire — the client treats absence as
// "data unavailable" (older paid carts may have nothing recorded for
// card-specific fields, since they were not persisted before this change).
type CartForCheckoutResponse struct {
	ID                 string                          `json:"id"`
	Token              string                          `json:"token"`
	Status             string                          `json:"status"`
	CustomerEmail      *string                         `json:"customerEmail"`
	PaymentStatus      string                          `json:"paymentStatus"`
	CheckoutURL        *string                         `json:"checkoutUrl"`
	PlatformHandle     string                          `json:"platformHandle"`
	AllowEdit          bool                            `json:"allowEdit"`
	MaxQuantityPerItem int                             `json:"maxQuantityPerItem"`
	ExpiresAt          *time.Time                      `json:"expiresAt"`
	PaidAt             *time.Time                      `json:"paidAt,omitempty"`
	CreatedAt          time.Time                       `json:"createdAt"`
	Event              CartEventInfo                   `json:"event"`
	Store              CartStoreInfo                   `json:"store"`
	Items              []CartItemResponse              `json:"items"`
	Summary            CartSummary                     `json:"summary"`
	Shipping           *CartShippingSelection          `json:"shipping,omitempty"`
	Customer           *CheckoutCustomerInfo           `json:"customer,omitempty"`
	ShippingAddress    *CheckoutShippingAddressInfo    `json:"shippingAddress,omitempty"`
	Payment            *CheckoutPaymentInfo            `json:"payment,omitempty"`
	// True when Customer / ShippingAddress were prefilled from the same buyer's
	// previous paid cart (returning-buyer flow). Frontend uses it to render the
	// "olá de novo" banner above the form.
	IsReturningCustomer bool                           `json:"isReturningCustomer,omitempty"`
}

// CheckoutCustomerInfo is the buyer identity captured at checkout. Exposed
// only after the cart is paid.
type CheckoutCustomerInfo struct {
	Name     string `json:"name"`
	Document string `json:"document"`
	Phone    string `json:"phone,omitempty"`
	Email    string `json:"email"`
}

// CheckoutShippingAddressInfo is the delivery address recorded at checkout.
// Exposed only after the cart is paid.
type CheckoutShippingAddressInfo struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

// CheckoutPaymentInfo is the payment confirmation snapshot for a paid cart.
//
// `method` is the public-facing value: "pix" or "card". `paymentId` is the
// gateway transaction id (Mercado Pago payment id, Pagar.me order id, etc.) —
// safe to expose: the customer / suporte uses it to look the payment up.
// CardBrand / LastFourDigits / Installments / AuthorizationCode are only set
// for card payments processed through the transparent checkout — missing on
// PIX, on redirect-checkout flows, and on carts paid before this field set
// existed. Older paid carts may also be missing `paymentId` if they predate
// the transparent flow.
type CheckoutPaymentInfo struct {
	Method            string    `json:"method"`
	PaidAt            time.Time `json:"paidAt"`
	PaymentID         string    `json:"paymentId,omitempty"`
	Installments      int       `json:"installments,omitempty"`
	CardBrand         string    `json:"cardBrand,omitempty"`
	LastFourDigits    string    `json:"lastFourDigits,omitempty"`
	AuthorizationCode string    `json:"authorizationCode,omitempty"`
}

// CartEventInfo contains event info for the checkout page
type CartEventInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CartStoreInfo contains store info for the checkout page
type CartStoreInfo struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	LogoURL *string `json:"logoUrl"`
}

// CartItemResponse represents a cart item in the checkout response
type CartItemResponse struct {
	ID                 string  `json:"id"`
	ProductID          string  `json:"productId"`
	Name               string  `json:"name"`
	ImageURL           *string `json:"imageUrl"`
	Keyword            *string `json:"keyword"`
	Quantity           int     `json:"quantity"`
	UnitPrice          int64   `json:"unitPrice"`
	TotalPrice         int64   `json:"totalPrice"`
	WaitlistedQuantity int     `json:"waitlistedQuantity"`
}

// CartSummary contains the cart totals
type CartSummary struct {
	Subtotal         int64 `json:"subtotal"`
	ShippingCost     int64 `json:"shippingCost"`
	Total            int64 `json:"total"`
	TotalItems       int   `json:"totalItems"`
	HasShippingQuote bool  `json:"hasShippingQuote"`
}

// CartShippingSelection describes the freight option currently attached to the cart.
// All fields are zero when the customer has not yet chosen an option.
type CartShippingSelection struct {
	Provider      string `json:"provider"`  // integration name (melhor_envio | smartenvios | ...)
	ServiceID     string `json:"serviceId"` // opaque, provider-specific service id
	ServiceName   string `json:"serviceName"`
	Carrier       string `json:"carrier"`
	CostCents     int64  `json:"costCents"`     // what the customer is charged (0 when free_shipping)
	RealCostCents int64  `json:"realCostCents"` // real quote value (merchant visibility)
	DeadlineDays  int    `json:"deadlineDays"`
	FreeShipping  bool   `json:"freeShipping"`
}

// GenerateCheckoutRequest is the request for POST /api/public/checkout/:token
type GenerateCheckoutRequest struct {
	Email string `json:"email" validate:"required,email"`
}

// GenerateCheckoutResponse is the response for POST /api/public/checkout/:token
type GenerateCheckoutResponse struct {
	CheckoutURL string     `json:"checkoutUrl"`
	ExpiresAt   *time.Time `json:"expiresAt"`
}

// =============================================================================
// TRANSPARENT CHECKOUT DTOs
// =============================================================================

// GetCheckoutConfigResponse is the response for GET /api/public/checkout/:token/config
type GetCheckoutConfigResponse struct {
	Provider         string   `json:"provider"`
	PublicKey        string   `json:"publicKey"`
	AvailableMethods []string `json:"availableMethods"`
	TotalAmount      int64    `json:"totalAmount"`
	Currency         string   `json:"currency"`
}

// ProcessCardPaymentRequest is the request for POST /api/public/checkout/:token/card
type ProcessCardPaymentRequest struct {
	Email            string           `json:"email" validate:"required,email"`
	Token            string           `json:"token" validate:"required"`
	Installments     int              `json:"installments" validate:"required,min=1,max=12"`
	PaymentMethodID  string           `json:"paymentMethodId,omitempty"` // For Mercado Pago
	IssuerID         string           `json:"issuerId,omitempty"`        // For Mercado Pago
	DeviceID         string           `json:"deviceId,omitempty"`        // For fraud prevention
	CustomerName     string           `json:"customerName" validate:"required"`
	CustomerDocument string           `json:"customerDocument" validate:"required"`
	CustomerPhone    string           `json:"customerPhone,omitempty"`
	ShippingAddress  *ShippingAddress `json:"shippingAddress" validate:"required"`
}

// ProcessCardPaymentResponse is the response for POST /api/public/checkout/:token/card
type ProcessCardPaymentResponse struct {
	PaymentID         string `json:"paymentId"`
	Status            string `json:"status"`
	StatusDetail      string `json:"statusDetail,omitempty"`
	Message           string `json:"message"`
	Amount            int64  `json:"amount"`
	Installments      int    `json:"installments"`
	LastFourDigits    string `json:"lastFourDigits,omitempty"`
	CardBrand         string `json:"cardBrand,omitempty"`
	AuthorizationCode string `json:"authorizationCode,omitempty"`
}

// GeneratePixRequest is the request for POST /api/public/checkout/:token/pix
type GeneratePixRequest struct {
	Email            string           `json:"email" validate:"required,email"`
	CustomerName     string           `json:"customerName" validate:"required"`
	CustomerDocument string           `json:"customerDocument" validate:"required"`
	CustomerPhone    string           `json:"customerPhone,omitempty"`
	ShippingAddress  *ShippingAddress `json:"shippingAddress" validate:"required"`
}

// ShippingAddress is the delivery address supplied by the customer at checkout.
// Persisted on the cart and forwarded to the ERP when the paid sales order is created.
type ShippingAddress struct {
	ZipCode      string `json:"zipCode" validate:"required"`
	Street       string `json:"street" validate:"required"`
	Number       string `json:"number" validate:"required"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood" validate:"required"`
	City         string `json:"city" validate:"required"`
	State        string `json:"state" validate:"required,len=2"`
}

// GeneratePixResponse is the response for POST /api/public/checkout/:token/pix
type GeneratePixResponse struct {
	PaymentID  string    `json:"paymentId"`
	QRCode     string    `json:"qrCode"`
	QRCodeText string    `json:"qrCodeText"`
	Amount     int64     `json:"amount"`
	ExpiresAt  time.Time `json:"expiresAt"`
	TicketURL  string    `json:"ticketUrl,omitempty"`
}

// GetPaymentStatusResponse is the response for GET /api/public/checkout/:token/status
type GetPaymentStatusResponse struct {
	Status        string     `json:"status"`
	PaymentStatus string     `json:"paymentStatus"`
	PaidAt        *time.Time `json:"paidAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}

// =============================================================================
// SERVICE INPUT/OUTPUT (Service layer)
// =============================================================================

// GetCartForCheckoutInput is the input for GetCartForCheckout service method
type GetCartForCheckoutInput struct {
	Token string
}

// GetCartForCheckoutOutput is the output for GetCartForCheckout service method.
//
// Customer / ShippingAddress / Payment are only set when Cart.PaymentStatus is
// "paid" — see Service.GetCartForCheckout. The handler propagates them as-is
// to the public response so unpaid carts never leak PII via the public token.
type GetCartForCheckoutOutput struct {
	Cart            CartDetails
	Items           []CartItemDetails
	Customer        *CartCustomerInfo
	ShippingAddress *CartShippingAddressInfo
	Payment         *CartPaymentInfo
}

// CartDetails contains the cart data with event/store info
type CartDetails struct {
	ID                  string
	EventID             string
	PlatformUserID      string
	PlatformHandle      string
	Token               string
	Status              string
	CheckoutURL         *string
	CheckoutID          *string
	CustomerEmail       *string
	PaymentStatus       string
	PaidAt              *time.Time
	CreatedAt           time.Time
	ExpiresAt           *time.Time
	EventTitle          string
	EventFreeShipping   bool
	StoreID             string
	StoreName           string
	StoreLogoURL        *string
	AllowEdit           bool
	MaxQuantityPerItem  int
	Shipping            *CartShippingSelection
	// Set by the service when Customer / ShippingAddress on this output came
	// from the buyer's prior paid cart (returning-buyer prefill) rather than
	// from the current cart's own paid receipt.
	IsReturningCustomer bool
}

// CartItemDetails contains a cart item with product info
type CartItemDetails struct {
	ID                 string
	CartID             string
	ProductID          string
	Quantity           int
	UnitPrice          int64
	WaitlistedQuantity int
	Name               string
	ImageURL           *string
	Keyword            *string
}

// GenerateCheckoutInput is the input for GenerateCheckout service method
type GenerateCheckoutInput struct {
	Token string
	Email string
}

// GenerateCheckoutOutput is the output for GenerateCheckout service method
type GenerateCheckoutOutput struct {
	CheckoutURL string
	ExpiresAt   *time.Time
}

// =============================================================================
// TRANSPARENT CHECKOUT SERVICE INPUT/OUTPUT
// =============================================================================

// GetCheckoutConfigInput is the input for GetCheckoutConfig service method
type GetCheckoutConfigInput struct {
	Token string
}

// GetCheckoutConfigOutput is the output for GetCheckoutConfig service method
type GetCheckoutConfigOutput struct {
	Provider         string
	PublicKey        string
	AvailableMethods []string
	TotalAmount      int64
	Currency         string
}

// ProcessCardPaymentInput is the input for ProcessCardPayment service method
type ProcessCardPaymentInput struct {
	Token            string
	Email            string
	CardToken        string
	Installments     int
	PaymentMethodID  string
	IssuerID         string
	DeviceID         string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string
	ShippingAddress  *ShippingAddress
}

// ProcessCardPaymentOutput is the output for ProcessCardPayment service method
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

// GeneratePixInput is the input for GeneratePix service method
type GeneratePixInput struct {
	Token            string
	Email            string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string
	ShippingAddress  *ShippingAddress
}

// GeneratePixOutput is the output for GeneratePix service method
type GeneratePixOutput struct {
	PaymentID  string
	QRCode     string
	QRCodeText string
	Amount     int64
	ExpiresAt  time.Time
	TicketURL  string
}

// GetPaymentStatusInput is the input for GetPaymentStatus service method
type GetPaymentStatusInput struct {
	Token string
}

// GetPaymentStatusOutput is the output for GetPaymentStatus service method
type GetPaymentStatusOutput struct {
	Status        string
	PaymentStatus string
	PaidAt        *time.Time
	Message       string
}

// =============================================================================
// SHIPPING DTOs
// =============================================================================

// ShippingQuoteRequest is the body for POST /api/public/checkout/:token/shipping-quote
type ShippingQuoteRequest struct {
	ZipCode string `json:"zipCode" validate:"required"`
}

// ShippingQuoteOptionResponse is a single carrier option returned from a quote.
type ShippingQuoteOptionResponse struct {
	ID             string `json:"id"`       // opaque service id returned by the provider
	Provider       string `json:"provider"` // integration name (melhor_envio | smartenvios | ...)
	Service        string `json:"service"`
	Carrier        string `json:"carrier"`
	CarrierLogoURL string `json:"carrierLogoUrl,omitempty"`
	PriceCents     int64  `json:"priceCents"`     // what customer will pay (0 when event free_shipping)
	RealPriceCents int64  `json:"realPriceCents"` // actual quote value
	DeadlineDays   int    `json:"deadlineDays"`
	Available      bool   `json:"available"`
	Error          string `json:"error,omitempty"`
}

// ShippingQuoteResponse is the body of a successful quote.
type ShippingQuoteResponse struct {
	QuotedAt     time.Time                     `json:"quotedAt"`
	FreeShipping bool                          `json:"freeShipping"`
	Options      []ShippingQuoteOptionResponse `json:"options"`
}

// SelectShippingMethodRequest is the body for PUT /api/public/checkout/:token/shipping-method.
// The zipCode is required because the address is only persisted on the cart
// at payment time — between quoting and picking a service only the frontend
// knows the destination.
//
// `provider` is required when the store has multiple shipping integrations
// active (quote options come from several providers with opaque ids that
// could collide in theory). When only one provider is active the backend
// will fill it in automatically.
type SelectShippingMethodRequest struct {
	Provider  string `json:"provider,omitempty"`
	ServiceID string `json:"serviceId" validate:"required"`
	ZipCode   string `json:"zipCode" validate:"required"`
}

// SelectShippingMethodResponse is the body returned after selecting a freight option.
type SelectShippingMethodResponse struct {
	Shipping CartShippingSelection `json:"shipping"`
	Summary  CartSummary           `json:"summary"`
}

// =============================================================================
// SHIPPING SERVICE IO
// =============================================================================

type QuoteShippingInput struct {
	Token      string
	ZipCode    string
	ServiceIDs []string // optional filter
	// Providers restricts the quote to a subset of integrations (by name).
	// Empty = query all active shipping integrations for the store.
	Providers []string
}

type QuoteShippingOutput struct {
	QuotedAt     time.Time
	FreeShipping bool
	Options      []ShippingQuoteOptionResponse
}

type SelectShippingMethodInput struct {
	Token     string
	Provider  string // optional when only one shipping integration is active
	ServiceID string
	ZipCode   string
}

type SelectShippingMethodOutput struct {
	Shipping CartShippingSelection
	Summary  CartSummary
}

// =============================================================================
// REPOSITORY TYPES
// =============================================================================

// CartRow represents a cart row from the database
type CartRow struct {
	ID                 string
	EventID            string
	PlatformUserID     string
	PlatformHandle     string
	Token              string
	Status             string
	CheckoutURL        *string
	CheckoutID         *string
	CheckoutExpiresAt  *time.Time
	CustomerEmail      *string
	PaymentStatus      string
	PaidAt             *time.Time
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	EventTitle         string
	EventFreeShipping  bool
	StoreID            string
	StoreName          string
	StoreLogoURL       *string
	AllowEdit          bool
	MaxQuantityPerItem int
	Shipping           *CartShippingSelection
}

// CartItemRow represents a cart item row from the database
type CartItemRow struct {
	ID                 string
	CartID             string
	ProductID          string
	Quantity           int
	UnitPrice          int64
	WaitlistedQuantity int
	Name               string
	ImageURL           *string
	Keyword            *string
}

// UpdateCheckoutParams contains parameters for updating cart checkout info
type UpdateCheckoutParams struct {
	CartID            string
	CheckoutURL       string
	CheckoutID        string
	CheckoutExpiresAt *time.Time
}
