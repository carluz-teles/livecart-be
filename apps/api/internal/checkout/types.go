package checkout

import (
	"time"
)

// =============================================================================
// REQUEST/RESPONSE DTOs (Handler layer)
// =============================================================================

// CartForCheckoutResponse is the response for GET /api/public/checkout/:token
type CartForCheckoutResponse struct {
	ID             string                `json:"id"`
	Token          string                `json:"token"`
	Status         string                `json:"status"`
	CustomerEmail  *string               `json:"customerEmail"`
	PaymentStatus  string                `json:"paymentStatus"`
	CheckoutURL    *string               `json:"checkoutUrl"`
	PlatformHandle string                `json:"platformHandle"`
	AllowEdit      bool                  `json:"allowEdit"`
	ExpiresAt      *time.Time            `json:"expiresAt"`
	CreatedAt      time.Time             `json:"createdAt"`
	Event          CartEventInfo         `json:"event"`
	Store          CartStoreInfo         `json:"store"`
	Items          []CartItemResponse    `json:"items"`
	Summary        CartSummary           `json:"summary"`
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
	ID         string  `json:"id"`
	ProductID  string  `json:"productId"`
	Name       string  `json:"name"`
	ImageURL   *string `json:"imageUrl"`
	Keyword    *string `json:"keyword"`
	Quantity   int     `json:"quantity"`
	UnitPrice  int64   `json:"unitPrice"`
	TotalPrice int64   `json:"totalPrice"`
	Waitlisted bool    `json:"waitlisted"`
}

// CartSummary contains the cart totals
type CartSummary struct {
	Subtotal   int64 `json:"subtotal"`
	TotalItems int   `json:"totalItems"`
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
	Email           string `json:"email" validate:"required,email"`
	Token           string `json:"token" validate:"required"`
	Installments    int    `json:"installments" validate:"required,min=1,max=12"`
	PaymentMethodID string `json:"paymentMethodId,omitempty"` // For Mercado Pago
	IssuerID        string `json:"issuerId,omitempty"`        // For Mercado Pago
	DeviceID        string `json:"deviceId,omitempty"`        // For fraud prevention
	CustomerName    string `json:"customerName,omitempty"`
	CustomerDocument string `json:"customerDocument,omitempty"`
	CustomerPhone   string `json:"customerPhone,omitempty"`
}

// ProcessCardPaymentResponse is the response for POST /api/public/checkout/:token/card
type ProcessCardPaymentResponse struct {
	PaymentID      string `json:"paymentId"`
	Status         string `json:"status"`
	StatusDetail   string `json:"statusDetail,omitempty"`
	Message        string `json:"message"`
	Amount         int64  `json:"amount"`
	Installments   int    `json:"installments"`
	LastFourDigits string `json:"lastFourDigits,omitempty"`
	CardBrand      string `json:"cardBrand,omitempty"`
}

// GeneratePixRequest is the request for POST /api/public/checkout/:token/pix
type GeneratePixRequest struct {
	Email            string `json:"email" validate:"required,email"`
	CustomerName     string `json:"customerName,omitempty"`
	CustomerDocument string `json:"customerDocument,omitempty"`
	CustomerPhone    string `json:"customerPhone,omitempty"`
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

// GetCartForCheckoutOutput is the output for GetCartForCheckout service method
type GetCartForCheckoutOutput struct {
	Cart  CartDetails
	Items []CartItemDetails
}

// CartDetails contains the cart data with event/store info
type CartDetails struct {
	ID              string
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	CheckoutURL     *string
	CheckoutID      *string
	CustomerEmail   *string
	PaymentStatus   string
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	EventTitle      string
	StoreID         string
	StoreName       string
	StoreLogoURL    *string
	AllowEdit       bool
}

// CartItemDetails contains a cart item with product info
type CartItemDetails struct {
	ID         string
	CartID     string
	ProductID  string
	Quantity   int
	UnitPrice  int64
	Waitlisted bool
	Name       string
	ImageURL   *string
	Keyword    *string
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
}

// ProcessCardPaymentOutput is the output for ProcessCardPayment service method
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

// GeneratePixInput is the input for GeneratePix service method
type GeneratePixInput struct {
	Token            string
	Email            string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string
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
// REPOSITORY TYPES
// =============================================================================

// CartRow represents a cart row from the database
type CartRow struct {
	ID                string
	EventID           string
	PlatformUserID    string
	PlatformHandle    string
	Token             string
	Status            string
	CheckoutURL       *string
	CheckoutID        *string
	CheckoutExpiresAt *time.Time
	CustomerEmail     *string
	PaymentStatus     string
	PaidAt            *time.Time
	CreatedAt         time.Time
	ExpiresAt         *time.Time
	EventTitle        string
	StoreID           string
	StoreName         string
	StoreLogoURL      *string
	AllowEdit         bool
}

// CartItemRow represents a cart item row from the database
type CartItemRow struct {
	ID         string
	CartID     string
	ProductID  string
	Quantity   int
	UnitPrice  int64
	Waitlisted bool
	Name       string
	ImageURL   *string
	Keyword    *string
}

// UpdateCheckoutParams contains parameters for updating cart checkout info
type UpdateCheckoutParams struct {
	CartID            string
	CheckoutURL       string
	CheckoutID        string
	CheckoutExpiresAt *time.Time
}
