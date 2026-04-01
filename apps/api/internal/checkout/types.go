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
